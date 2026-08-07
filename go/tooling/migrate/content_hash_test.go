package migrate

import "testing"

// TestContentHash pins the canonical injective encoding (writer-F2/sign-F5) so
// the console (which imports this exact function) and the apply tool can never
// drift, and a format change is a deliberate, visible break.
func TestContentHash(t *testing.T) {
	if got := ContentHash("up", "post", "pre", "down"); got != "d514736f2f182df57c65459a84b11592fba6579a3f184a54297f3cf26036e109" {
		t.Errorf("ContentHash 4-segment vector changed: %s", got)
	}
	// A zero-SQL migration (all segments empty) still hashes to a stable value.
	if got := ContentHash("", "", "", ""); got != "e6ecd712cc84f6ba8e6d4a8bdbab6ad62b5a7ea819a813a3eb2945a9bc230b7a" {
		t.Errorf("ContentHash empty vector changed: %s", got)
	}
	// Injectivity: moving a byte across a segment boundary changes the hash.
	if ContentHash("ab", "", "", "") == ContentHash("a", "b", "", "") {
		t.Error("ContentHash is not injective across segment boundaries")
	}
}
