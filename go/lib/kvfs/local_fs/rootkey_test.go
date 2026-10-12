package local_fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// A key that names the bucket root itself — empty, `.`, or one that cleans
// to it — or a sub-bucket directory is not an object: reads report
// ErrNotFound, and Delete refuses rather than removing the directory.
func TestLocalFS_RootAndDirectoryKeysAreNotObjects(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "bucket")
	if err := os.MkdirAll(filepath.Join(root, "avatars", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(root)

	for _, key := range []string{"", ".", "./", "avatars/..", "avatars/empty", "avatars"} {
		if _, err := d.Open(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", key, err)
		}
		if _, err := d.OpenSeekable(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("OpenSeekable(%q) = %v, want ErrNotFound", key, err)
		}
		if _, err := d.Stat(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want ErrNotFound", key, err)
		}
	}
	// An EMPTY bucket root and an empty sub-bucket are what os.Remove would
	// delete.
	empty := filepath.Join(t.TempDir(), "empty-bucket")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		d   *Driver
		key string
		dir string
	}{
		{New(empty), "", empty},
		{New(empty), ".", empty},
		{d, "avatars/empty", filepath.Join(root, "avatars", "empty")},
	} {
		if err := tc.d.Delete(ctx, tc.key); err == nil {
			t.Errorf("Delete(%q) succeeded", tc.key)
		}
		if _, err := os.Stat(tc.dir); err != nil {
			t.Errorf("Delete(%q) removed %s: %v", tc.key, tc.dir, err)
		}
	}
	if _, err := d.PutFromTempFile(ctx, ".", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error(`PutFromTempFile(".") succeeded`)
	}
}
