package local_fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// The staging area is where in-flight uploads are written; it is not part
// of the key space. A key under it MUST be refused like a key outside the
// root: reads report ErrNotFound, Delete errors and removes nothing.
func TestLocalFS_StagingAreaIsNotAnObjectKey(t *testing.T) {
	d := New(t.TempDir())
	dir, err := d.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "w17-upload-1")
	if err := os.WriteFile(staged, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{stagingDirName + "/w17-upload-1", "a/../" + stagingDirName + "/w17-upload-1"} {
		if _, err := d.Open(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", key, err)
		}
		if _, err := d.Stat(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want ErrNotFound", key, err)
		}
		if _, err := d.OpenSeekable(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("OpenSeekable(%q) = %v, want ErrNotFound", key, err)
		}
		if err := d.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) succeeded", key)
		}
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("the staged upload is gone: %v", err)
	}
}
