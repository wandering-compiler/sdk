package memory_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/kvfs/memory"
)

// TestDriver_Has — Has reports presence by key: true for a stored object,
// false for an absent one.
func TestDriver_Has(t *testing.T) {
	d := memory.New()
	d.PutBytes("a/b.txt", []byte("x"))

	if !d.Has("a/b.txt") {
		t.Error("Has should be true for a stored key")
	}
	if d.Has("missing") {
		t.Error("Has should be false for an absent key")
	}
}
