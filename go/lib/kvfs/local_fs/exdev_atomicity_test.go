//go:build unix

package local_fs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// crossFSDir returns a scratch directory that is GUARANTEED to sit on a
// different filesystem than `other`, so an os.Rename between the two
// really fails with EXDEV. It is the honest way to exercise
// PutFromTempFile's cross-device fallback: a same-filesystem stand-in
// never enters that branch at all.
//
// /dev/shm is a separate tmpfs mount in every Linux container and on
// every mainstream distro, while t.TempDir() lands on the container's
// overlay (or the host's root fs) — the exact shape of the shipped
// deployment the audit measured (bucket root on a mounted volume, /tmp
// on the container's own root). The test SKIPS rather than lies when the
// two happen to share a device.
func crossFSDir(t *testing.T, other string) string {
	t.Helper()
	const shm = "/dev/shm"
	if _, err := os.Stat(shm); err != nil {
		t.Skipf("no %s on this box: %v (cannot exercise the real EXDEV path)", shm, err)
	}
	if devOf(t, shm) == devOf(t, other) {
		t.Skipf("%s and %s share a device — no cross-filesystem pair available", shm, other)
	}
	dir, err := os.MkdirTemp(shm, "w17-xdev-*")
	if err != nil {
		t.Skipf("mkdtemp under %s: %v", shm, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func devOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("stat of %s carries no syscall.Stat_t", path)
	}
	return uint64(st.Dev) //nolint:unconvert // Dev is int32 on darwin, uint64 on linux
}

// stageOnto writes `body` into a fresh file inside dir and returns its path.
func stageOnto(t *testing.T, dir string, body []byte) string {
	t.Helper()
	f, err := os.CreateTemp(dir, "stage-*")
	if err != nil {
		t.Fatalf("create stage file: %v", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		t.Fatalf("write stage file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close stage file: %v", err)
	}
	return f.Name()
}

// TestPutFromTempFile_CrossFS_ReaderNeverSeesPartialBody is the
// LINEARIZABILITY pin on the kvfs.Driver contract quoted at
// kvfs.go:49-56 — "concurrent readers either see the previous value …
// or the new value, never a half-written body".
//
// It publishes a key, then republishes it repeatedly ACROSS A REAL
// FILESYSTEM BOUNDARY while a concurrent reader keeps opening the same
// key. Every completed read must equal one of the two whole bodies:
// no legal serial order of {put, read} can yield a prefix of either, an
// empty file, or a mix. A truncate-in-place fallback yields all three.
func TestPutFromTempFile_CrossFS_ReaderNeverSeesPartialBody(t *testing.T) {
	root := t.TempDir()
	stageDir := crossFSDir(t, root)

	d := New(root)
	const key = "avatars/ab/cd/object.bin"

	bodyA := bytes.Repeat([]byte{'A'}, 3<<20)
	bodyB := bytes.Repeat([]byte{'B'}, 2<<20)

	// Publish A first, same-filesystem (the uncontested happy path), so
	// the key is LIVE before any cross-fs republish starts.
	if _, err := d.PutFromTempFile(context.Background(), key, stageOnto(t, root, bodyA)); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	var (
		stop  = make(chan struct{})
		wg    sync.WaitGroup
		mu    sync.Mutex
		bad   []string
		reads int
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rc, err := d.Open(context.Background(), key)
			if err != nil {
				mu.Lock()
				bad = append(bad, "open: "+err.Error())
				mu.Unlock()
				return
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				mu.Lock()
				bad = append(bad, "read: "+err.Error())
				mu.Unlock()
				return
			}
			mu.Lock()
			reads++
			if !bytes.Equal(got, bodyA) && !bytes.Equal(got, bodyB) {
				bad = append(bad, describeTornRead(got, bodyA, bodyB))
			}
			n := len(bad)
			mu.Unlock()
			if n > 0 {
				return
			}
		}
	}()

	for i := range 20 {
		body := bodyB
		if i%2 == 1 {
			body = bodyA
		}
		tmp := stageOnto(t, stageDir, body)
		if _, err := d.PutFromTempFile(context.Background(), key, tmp); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("cross-fs put %d: %v", i, err)
		}
		_ = os.Remove(tmp)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if reads == 0 {
		t.Fatal("reader observed nothing — the race window was never sampled")
	}
	if len(bad) > 0 {
		t.Errorf("concurrent reader observed %d non-atomic state(s) across %d reads; first: %s",
			len(bad), reads, bad[0])
	}
}

// TestPutFromTempFile_PublishedModeIsBranchIndependent pins that an
// object's permissions do not depend on WHICH publish branch ran. The
// staging file both branches consume is an os.CreateTemp (0o600); the
// package documents 0o644 for published objects because they are public
// assets typically served by a separate static file server. Before the
// staging fix, containerised deployments always took the copy branch and
// its 0o644 — moving them onto the rename branch must not silently
// tighten the mode under them.
func TestPutFromTempFile_PublishedModeIsBranchIndependent(t *testing.T) {
	root := t.TempDir()
	d := New(root)

	if _, err := d.PutFromTempFile(context.Background(), "same-fs.bin", stageOnto(t, root, []byte("x"))); err != nil {
		t.Fatalf("same-fs put: %v", err)
	}
	assertMode(t, filepath.Join(root, "same-fs.bin"), 0o644)

	stageDir := crossFSDir(t, root)
	if _, err := d.PutFromTempFile(context.Background(), "cross-fs.bin", stageOnto(t, stageDir, []byte("x"))); err != nil {
		t.Fatalf("cross-fs put: %v", err)
	}
	assertMode(t, filepath.Join(root, "cross-fs.bin"), 0o644)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func describeTornRead(got, a, b []byte) string {
	switch {
	case len(got) == 0:
		return "read an EMPTY body (key truncated in place)"
	case bytes.HasPrefix(a, got):
		return "read a PREFIX of the A body (len " + itoa(len(got)) + " of " + itoa(len(a)) + ")"
	case bytes.HasPrefix(b, got):
		return "read a PREFIX of the B body (len " + itoa(len(got)) + " of " + itoa(len(b)) + ")"
	default:
		return "read a body matching neither value (len " + itoa(len(got)) + ")"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestPutFromTempFile_CrossFS_CopyErrorPreservesPublishedObject pins the
// ERROR path of the same fallback. Keys are content-addressed, so a
// re-upload of bytes that already exist targets a LIVE object other rows
// reference; a failed copy must leave that object exactly as it was.
//
// The failure is induced honestly: the staging path is a DIRECTORY on
// the other filesystem. The rename fails EXDEV (the mount check precedes
// every type check), the fallback then opens it fine and fails on the
// first read with EISDIR — a mid-copy error with the destination already
// opened for writing, which is precisely the disk-full shape.
func TestPutFromTempFile_CrossFS_CopyErrorPreservesPublishedObject(t *testing.T) {
	root := t.TempDir()
	stageDir := crossFSDir(t, root)

	d := New(root)
	const key = "avatars/ab/cd/object.bin"
	published := bytes.Repeat([]byte{'P'}, 4096)

	if _, err := d.PutFromTempFile(context.Background(), key, stageOnto(t, root, published)); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	unreadable := filepath.Join(stageDir, "a-directory")
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatalf("mkdir staging dir: %v", err)
	}

	if _, err := d.PutFromTempFile(context.Background(), key, unreadable); err == nil {
		t.Fatal("PutFromTempFile from an unreadable source = nil error, want failure")
	}

	rc, err := d.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("previously published object is gone after a failed put: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read published object: %v", err)
	}
	if !bytes.Equal(got, published) {
		t.Errorf("published object damaged by a failed put: len %d, want %d", len(got), len(published))
	}

	// The failed fallback must not leave its staging file behind in the
	// bucket either — a leaked partial next to the object is a slow leak
	// and an ambiguous artefact for an operator.
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(root, key)))
	if err != nil {
		t.Fatalf("read key dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("key dir holds %v, want only the object itself", names)
	}
}
