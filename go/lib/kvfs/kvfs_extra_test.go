package kvfs_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs"
)

// TestErrNotFound_Message pins the sentinel's user-facing text —
// drivers surface it verbatim and callers may log it, so the
// string is part of the package contract.
func TestErrNotFound_Message(t *testing.T) {
	if got := kvfs.ErrNotFound.Error(); got != "kvfs: key not found" {
		t.Errorf("ErrNotFound.Error() = %q, want %q", got, "kvfs: key not found")
	}
	if !errors.Is(kvfs.ErrNotFound, kvfs.ErrNotFound) {
		t.Error("ErrNotFound must satisfy errors.Is against itself")
	}
}

// failReader errors immediately, modelling a torn upload stream.
type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("stream broke") }

// TestHashFromReader_ReadError verifies the invariant that a read
// failure propagates: HashFromReader returns the error and an
// empty hash rather than a hash of the partial body.
func TestHashFromReader_ReadError(t *testing.T) {
	hash, n, err := kvfs.HashFromReader(failReader{})
	if err == nil {
		t.Fatal("expected error from failing reader")
	}
	if hash != "" {
		t.Errorf("hash should be empty on error, got %q", hash)
	}
	if n != 0 {
		t.Errorf("byte count should be 0 on immediate failure, got %d", n)
	}
}

// TestHashFromReader_Empty checks the zero-length edge: an empty
// reader hashes to the sha256 of the empty string with a zero
// byte count and no error.
func TestHashFromReader_Empty(t *testing.T) {
	hash, n, err := kvfs.HashFromReader(strings.NewReader(""))
	if err != nil {
		t.Fatalf("HashFromReader(empty): %v", err)
	}
	if n != 0 {
		t.Errorf("byte count = %d, want 0", n)
	}
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if hash != emptySHA {
		t.Errorf("empty hash = %q, want %q", hash, emptySHA)
	}
}

// TestBucketDepth_LargeRatioClamps drives the high end of the
// derivation: the largest representable ratio still lands within
// the [1,16] clamp window (uint64 capacity tops out near depth 8),
// so depth stays a small positive integer.
func TestBucketDepth_LargeRatioClamps(t *testing.T) {
	// max uint64 objects, 2 per bucket → ratio ~9.2e18 → depth ~8.
	d := kvfs.BucketDepth(^uint64(0), 2)
	if d < 1 || d > 16 {
		t.Errorf("BucketDepth(maxuint64, 2) = %d, want within [1,16]", d)
	}
}
