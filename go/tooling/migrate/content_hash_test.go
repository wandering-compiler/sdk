package migrate

import "testing"

// TestContentHash pins the canonical injective encoding (writer-F2/sign-F5) so
// the console (which imports this exact function) and the apply tool can never
// drift, and a format change is a deliberate, visible break.
//
// The vectors below are the ones this test carried BEFORE the chain existed,
// unchanged. That is the point: an empty predecessor emits the v1 encoding
// byte-for-byte, so every migration an older console stored keeps its hash and
// keeps verifying. If chaining had changed them, this test would have had to be
// rewritten — and rewriting it is exactly how a silent break gets normalised.
func TestContentHash(t *testing.T) {
	if got := ContentHash("up", "post", "pre", "down", "", nil, ""); got != "d514736f2f182df57c65459a84b11592fba6579a3f184a54297f3cf26036e109" {
		t.Errorf("ContentHash 4-segment vector changed: %s", got)
	}
	// A zero-SQL migration (all segments empty) still hashes to a stable value.
	if got := ContentHash("", "", "", "", "", nil, ""); got != "e6ecd712cc84f6ba8e6d4a8bdbab6ad62b5a7ea819a813a3eb2945a9bc230b7a" {
		t.Errorf("ContentHash empty vector changed: %s", got)
	}
	// Injectivity: moving a byte across a segment boundary changes the hash.
	if ContentHash("ab", "", "", "", "", nil, "") == ContentHash("a", "b", "", "", "", nil, "") {
		t.Error("ContentHash is not injective across segment boundaries")
	}
}

// T2-5 B11-1 — the predecessor is an INPUT to the hash, not a field beside it.
//
// This is the property the whole fix rests on. If prev only travelled next to
// content_sha256, whoever rewrites a migration's body rewrites its prev too and
// nothing downstream notices; because it is hashed IN, changing a migration
// changes its hash, which changes its successor's input, all the way up to the
// target whose hash the signed lock pins.
func TestContentHash_PredecessorIsHashedIn(t *testing.T) {
	body := func(prev string) string { return ContentHash("up", "post", "pre", "down", prev, nil, "") }

	root := body("")
	chained := body("aaaa")
	other := body("bbbb")

	if chained == root {
		t.Error("a chained migration must not hash like an unchained one — otherwise the link can be stripped for free")
	}
	if chained == other {
		t.Error("two different predecessors must produce different hashes")
	}

	// The version tags make the two encodings disjoint, so no chained input
	// can collide with an unchained one by construction (not by luck).
	if ContentHash("", "", "", "", "x", nil, "") == ContentHash("x", "", "", "", "", nil, "") {
		t.Error("v1 and v2 encodings collided — the version tag is not separating them")
	}
}

// A chain link must be sensitive to the WHOLE predecessor hash, not a prefix:
// the walk looks predecessors up by exact hash, and a truncation-tolerant link
// would let a near-miss resolve.
func TestContentHash_PredecessorIsNotTruncated(t *testing.T) {
	full := ContentHash("up", "", "", "", "0123456789abcdef", nil, "")
	short := ContentHash("up", "", "", "", "0123456789abcde", nil, "")
	if full == short {
		t.Error("predecessor hash length is not covered")
	}
}
