package restgw

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestStreamToTempFile pins the error arms: a tmpDir that's actually a file
// fails MkdirAll, a reader that errors fails the hash stream, and a body
// exceeding maxSize trips ErrUploadTooLarge. The happy path returns a hash.
func TestStreamToTempFile(t *testing.T) {
	dir := t.TempDir()

	// happy path → hash + temp path, no error.
	if _, path, err := streamToTempFile(strings.NewReader("hello"), dir, 100); err != nil || path == "" {
		t.Fatalf("happy = (%q,%v), want a temp path", path, err)
	}

	// tmpDir is a file → MkdirAll fails.
	fpath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(fpath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := streamToTempFile(strings.NewReader("x"), fpath, 0); err == nil {
		t.Error("tmpDir-is-a-file must fail MkdirAll")
	}

	// reader errors → stream fails.
	if _, _, err := streamToTempFile(errReader{}, dir, 0); err == nil {
		t.Error("failing reader must surface a stream error")
	}

	// body larger than maxSize → ErrUploadTooLarge.
	if _, _, err := streamToTempFile(strings.NewReader("abcdef"), dir, 3); !errors.Is(err, ErrUploadTooLarge) {
		t.Errorf("oversize body = %v, want ErrUploadTooLarge", err)
	}
}
