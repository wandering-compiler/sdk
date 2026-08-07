package memory_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
)

func TestMemory_PutOpenDelete(t *testing.T) {
	d := memory.New()
	ctx := context.Background()

	tmpPath := writeTempFile(t, []byte("hello world"))
	handle, err := d.PutFromTempFile(ctx, "k1", tmpPath)
	if err != nil {
		t.Fatalf("PutFromTempFile: %v", err)
	}
	if handle != "k1" {
		t.Errorf("handle = %q, want k1", handle)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file should be consumed; stat err = %v", err)
	}

	rc, err := d.Open(ctx, "k1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "hello world" {
		t.Errorf("body = %q, want hello world", body)
	}

	if err := d.Delete(ctx, "k1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := d.Open(ctx, "k1"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open after delete = %v, want ErrNotFound", err)
	}
}

func TestMemory_Open_MissingKey(t *testing.T) {
	d := memory.New()
	if _, err := d.Open(context.Background(), "nope"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Open missing = %v, want ErrNotFound", err)
	}
}

func TestMemory_Delete_MissingKey_Idempotent(t *testing.T) {
	d := memory.New()
	if err := d.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("Delete missing returned %v, want nil (idempotent)", err)
	}
}

func TestMemory_PutBytes_HelperPath(t *testing.T) {
	d := memory.New()
	d.PutBytes("seeded", []byte("from-test"))
	rc, err := d.Open(context.Background(), "seeded")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "from-test" {
		t.Errorf("body = %q, want from-test", body)
	}
}

func TestMemory_Stat(t *testing.T) {
	d := memory.New()
	frozen := time.Date(2026, 5, 8, 9, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return frozen })
	d.PutBytes("k", []byte("eleven char"))
	info, err := d.Stat(context.Background(), "k")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 11 {
		t.Errorf("Size = %d, want 11", info.Size)
	}
	if !info.ModTime.Equal(frozen) {
		t.Errorf("ModTime = %v, want %v", info.ModTime, frozen)
	}
}

func TestMemory_Stat_MissingKey(t *testing.T) {
	d := memory.New()
	if _, err := d.Stat(context.Background(), "ghost"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("Stat missing = %v, want ErrNotFound", err)
	}
}

func TestMemory_OpenSeekable_SeekThenRead(t *testing.T) {
	d := memory.New()
	d.PutBytes("k", []byte("0123456789"))
	rsc, err := d.OpenSeekable(context.Background(), "k")
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

func TestMemory_OpenSeekable_MissingKey(t *testing.T) {
	d := memory.New()
	if _, err := d.OpenSeekable(context.Background(), "ghost"); !errors.Is(err, kvfs.ErrNotFound) {
		t.Errorf("OpenSeekable missing = %v, want ErrNotFound", err)
	}
}

// TestMemory_HighVolumeDistribution simulates 1000 uploads
// at depth 1 (the "100k expected, 1k per bucket" config) and
// asserts the memory driver stores them all under
// distinct keys. NO disk I/O — fixture-grade.
func TestMemory_HighVolumeDistribution(t *testing.T) {
	d := memory.New()
	const (
		nKeys     = 1000
		expected  = 100_000
		maxBucket = 1000
	)
	for i := 0; i < nKeys; i++ {
		// Object key = the hash of a synthetic "user id";
		// matches the production shape where the upload
		// handler hashes the file body to derive the key.
		objKey := kvfs.HashKey([]byte(string(rune('A')+rune(i%26)) + "/" + string(rune(i))))
		fullKey := kvfs.BuildKey("/uploads", objKey, expected, maxBucket)
		tmp := writeTempFile(t, []byte("payload"))
		if _, err := d.PutFromTempFile(context.Background(), fullKey, tmp); err != nil {
			t.Fatalf("PutFromTempFile %d: %v", i, err)
		}
	}
	// Memory driver holds every key independently — count
	// matches puts (no overwrite given the synthetic key
	// shape). If a future change collapses the layout
	// unintentionally, this catches it.
	if got := len(d.Keys()); got != nKeys {
		t.Errorf("stored keys = %d, want %d", got, nKeys)
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
