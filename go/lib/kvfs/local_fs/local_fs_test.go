package local_fs_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/local_fs"
)

func TestLocalFS_PutOpenDelete(t *testing.T) {
	root := t.TempDir()
	if err := local_fs.EnsureRoot(root); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	d := local_fs.New(root)
	ctx := context.Background()

	tmpPath := writeTempFile(t, []byte("hello world"))
	key := kvfs.BuildKeyAt("/uploads", "alice", 1)
	handle, err := d.PutFromTempFile(ctx, key, tmpPath)
	if err != nil {
		t.Fatalf("PutFromTempFile: %v", err)
	}
	if handle != key {
		t.Errorf("handle = %q, want %q (handle should equal the storage key)", handle, key)
	}
	// File actually landed under the FS root.
	finalPath := filepath.Join(root, filepath.FromSlash(key))
	if _, err := os.Stat(finalPath); err != nil {
		t.Errorf("expected file at %s: %v", finalPath, err)
	}
	// Tmp source must be gone after a successful rename.
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file should be moved; stat err = %v", err)
	}

	rc, err := d.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "hello world" {
		t.Errorf("body = %q, want hello world", body)
	}

	if err := d.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := d.Open(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open after delete = %v, want ErrNotFound", err)
	}
}

// TestLocalFS_PathTraversalContained is the SEC-02 regression guard.
// filepath.Join collapses `..`, so an un-sanitized key could escape the
// bucket root and read/write/delete arbitrary files. containedPath must
// reject every such key: reads report ErrNotFound (no boundary leak),
// writes/deletes hard-error, and the out-of-root file is never touched.
func TestLocalFS_PathTraversalContained(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	ctx := context.Background()

	// A secret sitting in the parent of the bucket root.
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	for _, key := range []string{
		"../secret.txt",
		"a/../../secret.txt",
		"uploads/../../../" + filepath.Base(filepath.Dir(root)) + "/secret.txt",
	} {
		if _, err := d.Open(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound (traversal must be contained)", key, err)
		}
		if _, err := d.OpenSeekable(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("OpenSeekable(%q) = %v, want ErrNotFound", key, err)
		}
		if _, err := d.Stat(ctx, key); !errors.Is(err, kvfs.ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want ErrNotFound", key, err)
		}
		if err := d.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) = nil, want a hard reject for an escaping key", key)
		}
		tmp := writeTempFile(t, []byte("evil"))
		if _, err := d.PutFromTempFile(ctx, key, tmp); err == nil {
			t.Errorf("PutFromTempFile(%q) = nil, want a hard reject", key)
		}
	}

	// The secret must be untouched and unread.
	if b, err := os.ReadFile(secret); err != nil || string(b) != "TOP SECRET" {
		t.Errorf("secret tampered: body=%q err=%v", b, err)
	}
}

func TestLocalFS_Open_MissingKey(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	if _, err := d.Open(context.Background(), "ghost"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open missing = %v, want ErrNotFound", err)
	}
}

func TestLocalFS_Delete_MissingKey_Idempotent(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	if err := d.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("Delete missing = %v, want nil (idempotent)", err)
	}
}

func TestLocalFS_LazyMkdir_OfDeepBucket(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	tmp := writeTempFile(t, []byte("deep"))
	// Deep key: depth 3 sub-bucket.
	key := kvfs.BuildKeyAt("/uploads/avatars", "bob", 3)
	if _, err := d.PutFromTempFile(context.Background(), key, tmp); err != nil {
		t.Fatalf("PutFromTempFile: %v", err)
	}
	// Verify the sub-bucket dirs exist.
	rel := filepath.Join(root, filepath.FromSlash(key))
	info, err := os.Stat(rel)
	if err != nil {
		t.Fatalf("Stat final: %v", err)
	}
	if info.Size() != 4 {
		t.Errorf("final size = %d, want 4 (\"deep\")", info.Size())
	}
}

func TestLocalFS_Stat(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	ctx := context.Background()
	tmp := writeTempFile(t, []byte("eleven char"))
	if _, err := d.PutFromTempFile(ctx, "things/a", tmp); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := d.Stat(ctx, "things/a")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 11 {
		t.Errorf("Size = %d, want 11", info.Size)
	}
	if info.ModTime.IsZero() {
		t.Errorf("ModTime should be populated, got zero value")
	}
}

func TestLocalFS_Stat_MissingKey(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	if _, err := d.Stat(context.Background(), "ghost"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Stat missing = %v, want ErrNotFound", err)
	}
}

func TestLocalFS_OpenSeekable_SeekThenRead(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	ctx := context.Background()
	tmp := writeTempFile(t, []byte("0123456789"))
	if _, err := d.PutFromTempFile(ctx, "k", tmp); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rsc, err := d.OpenSeekable(ctx, "k")
	if err != nil {
		t.Fatalf("OpenSeekable: %v", err)
	}
	defer func() { _ = rsc.Close() }()
	if _, err := rsc.Seek(5, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(rsc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "56789" {
		t.Errorf("body after seek = %q, want 56789", got)
	}
}

func TestLocalFS_OpenSeekable_MissingKey(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	if _, err := d.OpenSeekable(context.Background(), "ghost"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("OpenSeekable missing = %v, want ErrNotFound", err)
	}
}

func TestLocalFS_EnsureRoot_Empty_Rejected(t *testing.T) {
	if err := local_fs.EnsureRoot(""); err == nil {
		t.Errorf("EnsureRoot(empty) = nil, want error")
	}
}

func TestLocalFS_Put_EmptyKey_Rejected(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	tmp := writeTempFile(t, []byte("x"))
	if _, err := d.PutFromTempFile(context.Background(), "", tmp); err == nil {
		t.Errorf("PutFromTempFile empty key = nil, want error")
	}
}

// TestLocalFS_Uninitialised_AllMethodsReject pins that a driver built
// with an empty root (New("")) rejects every operation with a clear
// "not initialised" error instead of touching the filesystem root.
func TestLocalFS_Uninitialised_AllMethodsReject(t *testing.T) {
	d := local_fs.New("")
	ctx := context.Background()
	if _, err := d.PutFromTempFile(ctx, "k", "tmp"); err == nil {
		t.Error("PutFromTempFile on empty-root driver = nil, want error")
	}
	if _, err := d.Open(ctx, "k"); err == nil {
		t.Error("Open on empty-root driver = nil, want error")
	}
	if _, err := d.Stat(ctx, "k"); err == nil {
		t.Error("Stat on empty-root driver = nil, want error")
	}
	if _, err := d.OpenSeekable(ctx, "k"); err == nil {
		t.Error("OpenSeekable on empty-root driver = nil, want error")
	}
	if err := d.Delete(ctx, "k"); err == nil {
		t.Error("Delete on empty-root driver = nil, want error")
	}
}

// TestLocalFS_Put_MkdirParentFails pins that when the key's parent path
// collides with an existing regular file (so MkdirAll can't create the
// directory) Put surfaces the mkdir error rather than the rename one.
func TestLocalFS_Put_MkdirParentFails(t *testing.T) {
	root := t.TempDir()
	// Create a regular file at "blocker"; then a key under it forces
	// MkdirAll(root/blocker) to fail (not a directory).
	if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	d := local_fs.New(root)
	tmp := writeTempFile(t, []byte("body"))
	if _, err := d.PutFromTempFile(context.Background(), "blocker/child", tmp); err == nil {
		t.Error("Put under a file-blocked parent = nil, want mkdir error")
	}
}

// TestLocalFS_Put_RenameFails pins the non-cross-device rename error
// path: a tmp source that doesn't exist makes os.Rename fail with a
// plain ENOENT (not EXDEV), which Put wraps verbatim.
func TestLocalFS_Put_RenameFails(t *testing.T) {
	root := t.TempDir()
	d := local_fs.New(root)
	missingTmp := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := d.PutFromTempFile(context.Background(), "k", missingTmp); err == nil {
		t.Error("Put with missing tmp source = nil, want rename error")
	}
}

// TestLocalFS_Open_NonNotFoundError pins that an underlying error other
// than ErrNotExist (here: a path component that is a file, yielding
// ENOTDIR) is wrapped and propagated — NOT masked as ErrNotFound.
func TestLocalFS_Open_NonNotFoundError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	d := local_fs.New(root)
	ctx := context.Background()
	// "file/child" treats a regular file as a directory → ENOTDIR.
	if _, err := d.Open(ctx, "file/child"); err == nil || errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open(file/child) = %v, want a non-ErrNotFound error", err)
	}
	if _, err := d.Stat(ctx, "file/child"); err == nil || errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Stat(file/child) = %v, want a non-ErrNotFound error", err)
	}
	if _, err := d.OpenSeekable(ctx, "file/child"); err == nil || errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("OpenSeekable(file/child) = %v, want a non-ErrNotFound error", err)
	}
	if err := d.Delete(ctx, "file/child"); err == nil {
		t.Errorf("Delete(file/child) = nil, want a non-ErrNotExist error")
	}
}

func writeTempFile(t *testing.T, body []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "src")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	return p
}
