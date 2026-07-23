package local_fs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyFile pins the cross-filesystem rename fallback: bytes are
// streamed verbatim from src to a freshly-created dst.
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload-bytes"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "payload-bytes" {
		t.Errorf("dst = %q, want payload-bytes", got)
	}
}

// TestCopyFile_SrcMissing pins that an absent source surfaces the open
// error rather than producing a truncated dst.
func TestCopyFile_SrcMissing(t *testing.T) {
	dir := t.TempDir()
	if err := copyFile(filepath.Join(dir, "ghost"), filepath.Join(dir, "dst")); err == nil {
		t.Error("copyFile(missing src) = nil, want error")
	}
}

// TestCopyFile_DstUncreatable pins that a dst whose parent dir doesn't
// exist surfaces the create error.
func TestCopyFile_DstUncreatable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := copyFile(src, filepath.Join(dir, "nope", "dst")); err == nil {
		t.Error("copyFile to nonexistent dir = nil, want error")
	}
}
