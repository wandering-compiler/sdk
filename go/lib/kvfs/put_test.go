package kvfs_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	localfs "github.com/wandering-compiler/sdk/go/lib/kvfs/local_fs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
)

// Put is the derivation a business tier writes a connection with: the
// handle it returns must sit under the field's bucket path and sub-bucket
// depth exactly as an upload's would, and read back verbatim.
func TestPut_PlacesUnderTheFieldsLayoutAndReadsBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d := localfs.New(root)
	p := kvfs.Placement{BucketPath: "/invoices/pdf", ExpectedObjects: 1_000_000, MaxPerBucket: 1000}

	h, err := kvfs.Put(ctx, d, p, strings.NewReader("%PDF-1.7 body"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	parts := strings.Split(h, "/")
	// invoices/pdf/<2 hex>/<2 hex>/<hash>-<nonce>: depth 2 for 1M / 1k.
	if len(parts) != 5 || parts[0] != "invoices" || parts[1] != "pdf" || len(parts[2]) != 2 || len(parts[3]) != 2 {
		t.Fatalf("handle %q is not <bucket>/<2 hex>/<2 hex>/<object>", h)
	}
	objectKey := parts[4]
	if want := kvfs.BuildKey(p.BucketPath, objectKey, p.ExpectedObjects, p.MaxPerBucket); h != want {
		t.Errorf("handle %q, BuildKey of its object key = %q", h, want)
	}
	if want := kvfs.HashKey([]byte("%PDF-1.7 body")); !strings.HasPrefix(objectKey, want+"-") {
		t.Errorf("object key %q does not start with the body's sha256 %s", objectKey, want)
	}
	rc, err := d.Open(ctx, h)
	if err != nil {
		t.Fatalf("Open(handle): %v", err)
	}
	defer func() { _ = rc.Close() }()
	if b, _ := io.ReadAll(rc); string(b) != "%PDF-1.7 body" {
		t.Errorf("read back %q", b)
	}

	// Staged inside the driver's root, and nothing left behind there.
	staging, err := d.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(staging, "w17-upload-*")); len(left) != 0 {
		t.Errorf("staging files left behind: %v", left)
	}
}

// Two identical bodies are two objects: deleting one row's file must not
// delete another's.
func TestPut_IdenticalBodiesAreDistinctObjects(t *testing.T) {
	ctx := context.Background()
	d := memory.New()
	p := kvfs.Placement{BucketPath: "/exports"}
	a, err := kvfs.Put(ctx, d, p, strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := kvfs.Put(ctx, d, p, strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two Puts of one body share handle %q", a)
	}
	if err := d.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(ctx, b); err != nil {
		t.Errorf("deleting the first object took the second: %v", err)
	}
}

// A body past MaxSizeBytes stores nothing and leaves no staging file.
func TestPut_TooLargeStoresNothing(t *testing.T) {
	ctx := context.Background()
	d := memory.New()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	_, err := kvfs.Put(ctx, d, kvfs.Placement{BucketPath: "/x", MaxSizeBytes: 3}, strings.NewReader("abcd"))
	if !errors.Is(err, kvfs.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if keys := d.Keys(); len(keys) != 0 {
		t.Errorf("stored %v", keys)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("staging files left behind: %v", left)
	}
}

type failingPut struct{ kvfs.Driver }

func (failingPut) PutFromTempFile(context.Context, string, string) (string, error) {
	return "", errors.New("bucket gone")
}

// A failing driver surfaces its error and the staging file is removed.
func TestPut_DriverErrorCleansUp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	_, err := kvfs.Put(context.Background(), failingPut{memory.New()}, kvfs.Placement{BucketPath: "/x"}, strings.NewReader("b"))
	if err == nil || !strings.Contains(err.Error(), "bucket gone") {
		t.Fatalf("err = %v, want the driver's error", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("staging files left behind: %v", left)
	}
}
