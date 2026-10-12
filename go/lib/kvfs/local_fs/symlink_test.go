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

// symlinkFixture is a bucket root and a directory outside it holding a
// file the driver must never read, overwrite or delete.
type symlinkFixture struct {
	root, outside, secret string
	d                     *local_fs.Driver
}

func newSymlinkFixture(t *testing.T) symlinkFixture {
	t.Helper()
	base := t.TempDir()
	f := symlinkFixture{root: filepath.Join(base, "bucket"), outside: filepath.Join(base, "outside")}
	for _, dir := range []string{f.root, f.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.secret = filepath.Join(f.outside, "secret.txt")
	if err := os.WriteFile(f.secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.d = local_fs.New(f.root)
	return f
}

func (f symlinkFixture) link(t *testing.T, target, name string) {
	t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func (f symlinkFixture) secretIntact(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(f.secret)
	if err != nil || string(b) != "secret" {
		t.Fatalf("file outside the bucket changed: %q, %v", b, err)
	}
	entries, _ := os.ReadDir(f.outside)
	if len(entries) != 1 {
		t.Fatalf("directory outside the bucket gained entries: %v", entries)
	}
}

func stage(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A directory symlink inside the bucket that points out of it: no
// operation may reach through it.
func TestLocalFS_DirectorySymlinkOutOfRootIsContained(t *testing.T) {
	ctx := context.Background()
	f := newSymlinkFixture(t)
	f.link(t, f.outside, "avatars")

	if _, err := f.d.Open(ctx, "avatars/secret.txt"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open through the link = %v, want ErrNotFound", err)
	}
	if _, err := f.d.OpenSeekable(ctx, "avatars/secret.txt"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("OpenSeekable through the link = %v, want ErrNotFound", err)
	}
	if _, err := f.d.Stat(ctx, "avatars/secret.txt"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Stat through the link = %v, want ErrNotFound", err)
	}
	if _, err := f.d.PutFromTempFile(ctx, "avatars/secret.txt", stage(t, "overwritten")); err == nil {
		t.Error("Put through the link succeeded, want refusal")
	}
	if _, err := f.d.PutFromTempFile(ctx, "avatars/new/deep.txt", stage(t, "planted")); err == nil {
		t.Error("Put of a new key through the link succeeded, want refusal")
	}
	if err := f.d.Delete(ctx, "avatars/secret.txt"); err == nil {
		t.Error("Delete through the link succeeded, want refusal")
	}
	f.secretIntact(t)
}

// A symlink deeper in the key path, below a real sub-bucket.
func TestLocalFS_NestedSymlinkOutOfRootIsContained(t *testing.T) {
	ctx := context.Background()
	f := newSymlinkFixture(t)
	f.link(t, "../../outside", "a/b")

	if _, err := f.d.Open(ctx, "a/b/secret.txt"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open = %v, want ErrNotFound", err)
	}
	if _, err := f.d.PutFromTempFile(ctx, "a/b/x.txt", stage(t, "planted")); err == nil {
		t.Error("Put succeeded, want refusal")
	}
	f.secretIntact(t)
}

// A dangling directory symlink pointing out of the bucket: a put must not
// create the missing directories wherever it points.
func TestLocalFS_DanglingSymlinkOutOfRootIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newSymlinkFixture(t)
	f.link(t, filepath.Join(f.outside, "created"), "avatars")

	if _, err := f.d.PutFromTempFile(ctx, "avatars/x/y.txt", stage(t, "planted")); err == nil {
		t.Error("Put through a dangling link succeeded, want refusal")
	}
	f.secretIntact(t)
}

// A key that IS a symlink to a file outside: reads refuse it; delete
// removes the link, never the file it points at; a put replaces the link.
func TestLocalFS_ObjectSymlinkOutOfRootIsContained(t *testing.T) {
	ctx := context.Background()
	f := newSymlinkFixture(t)
	f.link(t, f.secret, "files/a.txt")

	if _, err := f.d.Open(ctx, "files/a.txt"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open = %v, want ErrNotFound", err)
	}
	if _, err := f.d.Stat(ctx, "files/a.txt"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Stat = %v, want ErrNotFound", err)
	}
	if _, err := f.d.PutFromTempFile(ctx, "files/a.txt", stage(t, "new body")); err != nil {
		t.Fatalf("Put over the link: %v", err)
	}
	f.secretIntact(t)
	rc, err := f.d.Open(ctx, "files/a.txt")
	if err != nil {
		t.Fatalf("Open after put: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "new body" {
		t.Errorf("object = %q, want the put body", b)
	}

	f.link(t, f.secret, "files/b.txt")
	if err := f.d.Delete(ctx, "files/b.txt"); err != nil {
		t.Fatalf("Delete of the link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "files", "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("link still present after Delete: %v", err)
	}
	f.secretIntact(t)
}

// Resolve-and-contain, not refuse: links that stay inside the bucket keep
// working, and so does a bucket root that is itself a symlink.
func TestLocalFS_SymlinksInsideRootStillWork(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root-link")
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("store", filepath.Join(real, "alias")); err != nil {
		t.Fatal(err)
	}
	d := local_fs.New(root)
	if _, err := d.PutFromTempFile(ctx, "alias/x.txt", stage(t, "hello")); err != nil {
		t.Fatalf("Put through an in-root link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "store", "x.txt")); err != nil {
		t.Fatalf("object not under the link target: %v", err)
	}
	if err := os.Symlink(filepath.Join(real, "store", "x.txt"), filepath.Join(real, "y.txt")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"alias/x.txt", "store/x.txt", "y.txt"} {
		rc, err := d.Open(ctx, key)
		if err != nil {
			t.Errorf("Open %s: %v", key, err)
			continue
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(b) != "hello" {
			t.Errorf("Open %s = %q", key, b)
		}
	}
	if err := d.Delete(ctx, "alias/x.txt"); err != nil {
		t.Errorf("Delete through an in-root link: %v", err)
	}
}

// The staging directory is inside the root too: one symlinked out of it
// must not become where uploads are written.
func TestLocalFS_StagingDirSymlinkOutOfRootIsRefused(t *testing.T) {
	f := newSymlinkFixture(t)
	f.link(t, f.outside, ".w17-staging")
	if dir, err := f.d.StagingDir(); err == nil {
		t.Errorf("StagingDir = %q, want refusal", dir)
	}
}
