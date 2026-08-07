//go:build dockertest

package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
)

const (
	minioUser = "minioadmin"
	minioPass = "minioadmin"
	testBkt   = "snaptest"
)

// TestSnapshotter_RoundTrip is the S2 s3 verify: seed objects → dump →
// wipe bucket → restore → objects + bodies preserved. Runs a throwaway
// minio. Gated by `//go:build dockertest`.
func TestSnapshotter_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	endpoint := startThrowawayMinio(ctx, t)
	// minio uses static creds; the SDK default chain reads these env vars.
	t.Setenv("AWS_ACCESS_KEY_ID", minioUser)
	t.Setenv("AWS_SECRET_ACCESS_KEY", minioPass)
	t.Setenv("AWS_REGION", "us-east-1")

	cli := minioClient(ctx, t, endpoint)
	if _, err := cli.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(testBkt)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	seed := map[string]string{
		"config/app.json":  `{"k":"v"}`,
		"seed/users.csv":   "id,name\n1,a\n",
		"nested/a/b/c.txt": "deep",
	}
	for k, v := range seed {
		if _, err := cli.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(testBkt), Key: aws.String(k), Body: strings.NewReader(v),
		}); err != nil {
			t.Fatalf("seed %q: %v", k, err)
		}
	}

	dsn := fmt.Sprintf("s3://%s?endpoint=%s&region=us-east-1", testBkt, endpoint)
	snap, err := s3.NewSnapshotter(dsn)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}

	var dump bytes.Buffer
	if err := snap.Dump(ctx, &dump); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if dump.Len() == 0 {
		t.Fatal("Dump empty")
	}

	// Wipe every object.
	wipeBucket(ctx, t, cli)
	if listCount(ctx, t, cli) != 0 {
		t.Fatal("wipe left objects")
	}

	if err := snap.Restore(ctx, bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got := listCount(ctx, t, cli); got != len(seed) {
		t.Fatalf("restored %d objects, want %d", got, len(seed))
	}
	for k, want := range seed {
		out, err := cli.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(testBkt), Key: aws.String(k)})
		if err != nil {
			t.Errorf("get %q: %v", k, err)
			continue
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(out.Body)
		_ = out.Body.Close()
		if buf.String() != want {
			t.Errorf("%q body = %q, want %q", k, buf.String(), want)
		}
	}
}

func minioClient(ctx context.Context, t *testing.T, endpoint string) *awss3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	return awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(endpoint)
	})
}

func wipeBucket(ctx context.Context, t *testing.T, cli *awss3.Client) {
	t.Helper()
	out, err := cli.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(testBkt)})
	if err != nil {
		t.Fatalf("list for wipe: %v", err)
	}
	for _, o := range out.Contents {
		if _, err := cli.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(testBkt), Key: o.Key}); err != nil {
			t.Fatalf("delete %q: %v", aws.ToString(o.Key), err)
		}
	}
}

func listCount(ctx context.Context, t *testing.T, cli *awss3.Client) int {
	t.Helper()
	out, err := cli.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(testBkt)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(out.Contents)
}

func startThrowawayMinio(ctx context.Context, t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "-P",
		"-e", "MINIO_ROOT_USER="+minioUser,
		"-e", "MINIO_ROOT_PASSWORD="+minioPass,
		"minio/minio", "server", "/data",
	).Output()
	if err != nil {
		t.Skipf("docker run failed (no docker?): %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "stop", id).Run() })

	portOut, err := exec.CommandContext(ctx, "docker", "port", id, "9000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	first := strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0]
	port := first[strings.LastIndex(first, ":")+1:]
	endpoint := "http://127.0.0.1:" + port

	// Poll the minio health endpoint via a throwaway client list call.
	os.Setenv("AWS_ACCESS_KEY_ID", minioUser)
	os.Setenv("AWS_SECRET_ACCESS_KEY", minioPass)
	cli := minioClient(ctx, t, endpoint)
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := cli.ListBuckets(ctx, &awss3.ListBucketsInput{})
		if err == nil {
			return endpoint
		}
		if time.Now().After(deadline) {
			t.Fatalf("minio not ready: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
