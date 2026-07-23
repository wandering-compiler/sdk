package s3_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
)

// snapshotterFor stands up the fake S3 endpoint (with the same AWS env the
// Applier tests use) and returns a Snapshotter pointed at it.
func snapshotterFor(t *testing.T, f *fakeS3) *s3.Snapshotter {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_required")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	dsn := "s3://bucket?endpoint=" + url.QueryEscape(srv.URL) + "&region=us-east-1"
	s, err := s3.NewSnapshotter(dsn)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	return s
}

// Dump mirrors the whole bucket; Restore PUTs every record back. The
// round-trip against two fakes proves both directions over real HTTP.
func TestSnapshotter_DumpRestoreRoundTrip(t *testing.T) {
	src := newFakeS3()
	src.objects["seed/a.json"] = []byte(`{"a":1}`)
	src.objects["seed/b.json"] = []byte(`{"b":2}`)
	src.objects["config/x.txt"] = []byte("hello")

	var buf bytes.Buffer
	if err := snapshotterFor(t, src).Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("Dump produced no bytes")
	}

	dst := newFakeS3()
	if err := snapshotterFor(t, dst).Restore(context.Background(), &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, k := range []string{"seed/a.json", "seed/b.json", "config/x.txt"} {
		got, ok := dst.objects[k]
		if !ok {
			t.Errorf("restored bucket missing %q", k)
			continue
		}
		if !bytes.Equal(got, src.objects[k]) {
			t.Errorf("restored %q = %q, want %q", k, got, src.objects[k])
		}
	}
}

// Restore surfaces a PutObject failure on a record.
func TestSnapshotter_RestorePutError(t *testing.T) {
	src := newFakeS3()
	src.objects["seed/a.json"] = []byte("{}")
	var buf bytes.Buffer
	if err := snapshotterFor(t, src).Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	dst := newFakeS3()
	dst.failPut["seed/a.json"] = 500
	err := snapshotterFor(t, dst).Restore(context.Background(), &buf)
	if err == nil || !strings.Contains(err.Error(), "PutObject") {
		t.Fatalf("want PutObject error, got %v", err)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, context.Canceled }

// Dump's encode-error arm: a failing writer makes the per-object gob encode
// fail after the object is listed + fetched.
func TestSnapshotter_DumpEncodeError(t *testing.T) {
	f := newFakeS3()
	f.objects["seed/a.json"] = []byte("{}")
	if err := snapshotterFor(t, f).Dump(context.Background(), errWriter{}); err == nil {
		t.Fatal("want encode error from a failing writer")
	}
}

// Dump surfaces a ListObjectsV2 failure.
func TestSnapshotter_DumpListError(t *testing.T) {
	f := newFakeS3()
	f.failList = true
	err := snapshotterFor(t, f).Dump(context.Background(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "ListObjectsV2") {
		t.Fatalf("want ListObjectsV2 error, got %v", err)
	}
}

// Dump surfaces a per-object GetObject failure (getObject error arm).
func TestSnapshotter_DumpGetError(t *testing.T) {
	f := newFakeS3()
	f.objects["only.json"] = []byte("{}")
	f.failGet["only.json"] = 500
	err := snapshotterFor(t, f).Dump(context.Background(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "GetObject") {
		t.Fatalf("want GetObject error, got %v", err)
	}
}

// Restore surfaces a malformed gob stream (decode error arm); an empty
// stream is a clean EOF no-op.
func TestSnapshotter_RestoreDecodeError(t *testing.T) {
	f := newFakeS3()
	s := snapshotterFor(t, f)
	if err := s.Restore(context.Background(), strings.NewReader("not-a-gob-stream")); err == nil {
		t.Fatal("want decode error on a malformed stream")
	}
	if err := s.Restore(context.Background(), bytes.NewReader(nil)); err != nil {
		t.Fatalf("empty stream should be a clean no-op, got %v", err)
	}
}
