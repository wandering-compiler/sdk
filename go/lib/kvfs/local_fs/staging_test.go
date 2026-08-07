//go:build unix

package local_fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// TestDriver_StagingDir_IsOnTheBucketFilesystem pins the second half of
// the cross-device story: a driver that owns a local bucket root must be
// able to TELL the upload handler where to stage, so the publish is a
// same-filesystem rename and the copy fallback is never entered at all.
//
// The assertion is the property that matters — same st_dev as the bucket
// root — not merely "some path was returned".
func TestDriver_StagingDir_IsOnTheBucketFilesystem(t *testing.T) {
	root := t.TempDir()
	var d kvfs.Driver = New(root)

	sp, ok := d.(kvfs.StagingDirProvider)
	if !ok {
		t.Fatal("local_fs.Driver does not implement kvfs.StagingDirProvider")
	}
	dir, err := sp.StagingDir()
	if err != nil {
		t.Fatalf("StagingDir: %v", err)
	}
	if !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		t.Errorf("StagingDir = %q, want a path under the bucket root %q", dir, root)
	}
	if devOf(t, dir) != devOf(t, root) {
		t.Errorf("StagingDir %q is on a different filesystem than the bucket root %q", dir, root)
	}
	// It must exist and be usable immediately — the handler creates its
	// temp file there without further ceremony.
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("StagingDir %q is not an existing directory (err=%v)", dir, err)
	}
	// And a rename from it into the bucket must be a plain same-fs rename.
	src := filepath.Join(dir, "probe")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if err := os.Rename(src, filepath.Join(root, "probe")); err != nil {
		t.Errorf("rename from StagingDir into the bucket root failed: %v", err)
	}
}

// TestDriver_StagingDir_Unwritable pins the error path: a bucket root
// that cannot host the staging dir surfaces an error rather than a path
// the caller would then fail on.
func TestDriver_StagingDir_Unwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0o555 dir is still writable, the assertion would be a no-op")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	if _, err := New(root).StagingDir(); err == nil {
		t.Error("StagingDir on an unwritable root = nil error, want failure")
	}
}

// TestDriver_StagingDir_Uninitialised pins the empty-root guard shared
// with every other method on the driver.
func TestDriver_StagingDir_Uninitialised(t *testing.T) {
	if _, err := (&Driver{}).StagingDir(); err == nil {
		t.Error("StagingDir on an uninitialised driver = nil error, want failure")
	}
}
