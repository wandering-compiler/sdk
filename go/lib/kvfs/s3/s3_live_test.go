package s3_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	kvs3 "github.com/wandering-compiler/sdk/go/lib/kvfs/s3"
)

// Live against a real S3-compatible store: W17_S3_ENDPOINT (e.g. a MinIO at
// http://minio:9000) with AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY set.
// W17_REQUIRE_LIVE_S3=1 turns a missing endpoint into a failure, so CI cannot
// go green by skipping the only tests that touch the store.
func liveDriver(t *testing.T, prefix string) (*kvs3.Driver, *awss3.Client, string) {
	t.Helper()
	ep := os.Getenv("W17_S3_ENDPOINT")
	if ep == "" {
		if os.Getenv("W17_REQUIRE_LIVE_S3") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_S3=1 but W17_S3_ENDPOINT is unset")
		}
		t.Skip("W17_S3_ENDPOINT unset — no S3-compatible store to test against")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	raw := awss3.NewFromConfig(cfg, func(o *awss3.Options) { o.UsePathStyle = true; o.BaseEndpoint = aws.String(ep) })
	bucket := "kvfs-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	if len(bucket) > 63 {
		bucket = bucket[:63]
	}
	bucket = strings.TrimRight(bucket, "-")
	if _, err := raw.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil &&
		!strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
		t.Fatalf("create bucket: %v", err)
	}
	dsn := "s3://" + bucket
	if prefix != "" {
		dsn += "/" + prefix
	}
	d, err := kvs3.New(ctx, dsn+"?endpoint="+ep)
	if err != nil {
		t.Fatal(err)
	}
	return d, raw, bucket
}

func stage(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload.tmp")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLive_PutOpenStatDelete(t *testing.T) {
	d, _, _ := liveDriver(t, "")
	ctx := context.Background()
	body := "hello, object store"
	tmp := stage(t, body)
	h, err := d.PutFromTempFile(ctx, "a/b/c.txt", tmp)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if h != "a/b/c.txt" {
		t.Errorf("handle = %q, want the key", h)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging file is still there after a successful put (%v)", err)
	}
	info, err := d.Stat(ctx, h)
	if err != nil || info.Size != int64(len(body)) || info.ModTime.IsZero() {
		t.Fatalf("stat = %+v, %v", info, err)
	}
	rc, err := d.Open(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != body {
		t.Errorf("open read %q", got)
	}
	if err := d.Delete(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, h); err != nil {
		t.Errorf("a second delete of the same key failed: %v", err)
	}
	if _, err := d.Stat(ctx, h); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("stat after delete: %v, want ErrNotFound", err)
	}
}

// A missing key is kvfs.ErrNotFound on every read path — the download handler
// answers 404 from that sentinel and anything else becomes a 500.
func TestLive_MissingIsErrNotFound(t *testing.T) {
	d, _, _ := liveDriver(t, "")
	ctx := context.Background()
	if _, err := d.Open(ctx, "nope"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open: %v", err)
	}
	if _, err := d.Stat(ctx, "nope"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Stat: %v", err)
	}
	if _, err := d.OpenSeekable(ctx, "nope"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("OpenSeekable: %v", err)
	}
}

// The download handler serves through http.ServeContent, which seeks; a Range
// request must get exactly its bytes.
func TestLive_RangeThroughServeContent(t *testing.T) {
	d, _, _ := liveDriver(t, "")
	ctx := context.Background()
	body := strings.Repeat("0123456789", 1000)
	h, err := d.PutFromTempFile(ctx, "range.bin", stage(t, body))
	if err != nil {
		t.Fatal(err)
	}
	rs, err := d.OpenSeekable(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=4995-5004")
	rec := httptest.NewRecorder()
	http.ServeContent(rec, req, "range.bin", time.Time{}, rs)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != body[4995:5005] {
		t.Fatalf("range: %d %q, want 206 %q", rec.Code, rec.Body.String(), body[4995:5005])
	}
	// A second, non-adjacent range on the same reader.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=9990-")
	rec = httptest.NewRecorder()
	http.ServeContent(rec, req, "range.bin", time.Time{}, rs)
	if rec.Body.String() != body[9990:] {
		t.Errorf("tail range = %q", rec.Body.String())
	}
}

// The prefix is applied to every key, and only inside the driver: the handle
// the caller stores stays the key it asked for.
func TestLive_PrefixIsInsideTheBucket(t *testing.T) {
	d, raw, bucket := liveDriver(t, "uploads/v1")
	ctx := context.Background()
	h, err := d.PutFromTempFile(ctx, "img/x.png", stage(t, "png"))
	if err != nil {
		t.Fatal(err)
	}
	if h != "img/x.png" {
		t.Errorf("handle = %q", h)
	}
	if _, err := raw.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("uploads/v1/img/x.png")}); err != nil {
		t.Errorf("the object is not under the prefix: %v", err)
	}
}

// A failed put leaves the staging file for the caller, per the Driver contract.
func TestLive_FailedPutKeepsTheStagingFile(t *testing.T) {
	d, _, _ := liveDriver(t, "")
	ctx := context.Background()
	missing, err := kvs3.New(ctx, "s3://kvfs-no-such-bucket-w17?endpoint="+os.Getenv("W17_S3_ENDPOINT"))
	if err != nil {
		t.Fatal(err)
	}
	_ = d
	tmp := stage(t, "kept")
	if _, err := missing.PutFromTempFile(ctx, "k", tmp); err == nil {
		t.Fatal("put into a bucket that does not exist succeeded")
	}
	if b, err := os.ReadFile(tmp); err != nil || !bytes.Equal(b, []byte("kept")) {
		t.Errorf("the staging file did not survive a failed put: %v", err)
	}
}
