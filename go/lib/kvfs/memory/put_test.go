package memory_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
)

// TestPutFromTempFile — reads the temp file, stores under key (handle == key),
// consumes (removes) the temp file; a missing temp path surfaces the read error.
func TestPutFromTempFile(t *testing.T) {
	d := memory.New()
	tmp := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(tmp, []byte("body-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := d.PutFromTempFile(context.Background(), "obj/1", tmp)
	if err != nil {
		t.Fatalf("PutFromTempFile: %v", err)
	}
	if h != "obj/1" {
		t.Errorf("handle = %q, want obj/1", h)
	}
	if !d.Has("obj/1") {
		t.Error("object not stored")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp file should be consumed, stat err = %v", err)
	}
	rc, err := d.Open(context.Background(), "obj/1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "body-bytes" {
		t.Errorf("stored body = %q", got)
	}

	// Missing temp path → read error.
	if _, err := d.PutFromTempFile(context.Background(), "obj/2", filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing temp file should error")
	}
}

// TestPut_ZeroDriverInitialisesMap — a zero-value Driver lazily allocates its
// object map on first put (the nil-map guard), so PutBytes works without New().
func TestPut_ZeroDriverInitialisesMap(t *testing.T) {
	var d memory.Driver
	d.PutBytes("k", []byte("x"))
	if !d.Has("k") {
		t.Error("zero Driver did not store after PutBytes")
	}
}
