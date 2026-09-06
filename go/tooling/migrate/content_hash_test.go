package migrate

import (
	"os"
	"strings"
	"testing"
)

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
	if got := ContentHash("up", "post", "pre", "down", "", nil, "", ""); got != "d514736f2f182df57c65459a84b11592fba6579a3f184a54297f3cf26036e109" {
		t.Errorf("ContentHash 4-segment vector changed: %s", got)
	}
	// A zero-SQL migration (all segments empty) still hashes to a stable value.
	if got := ContentHash("", "", "", "", "", nil, "", ""); got != "e6ecd712cc84f6ba8e6d4a8bdbab6ad62b5a7ea819a813a3eb2945a9bc230b7a" {
		t.Errorf("ContentHash empty vector changed: %s", got)
	}
	// Injectivity: moving a byte across a segment boundary changes the hash.
	if ContentHash("ab", "", "", "", "", nil, "", "") == ContentHash("a", "b", "", "", "", nil, "", "") {
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
	body := func(prev string) string { return ContentHash("up", "post", "pre", "down", prev, nil, "", "") }

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
	if ContentHash("", "", "", "", "x", nil, "", "") == ContentHash("x", "", "", "", "", nil, "", "") {
		t.Error("v1 and v2 encodings collided — the version tag is not separating them")
	}
}

// A chain link must be sensitive to the WHOLE predecessor hash, not a prefix:
// the walk looks predecessors up by exact hash, and a truncation-tolerant link
// would let a near-miss resolve.
func TestContentHash_PredecessorIsNotTruncated(t *testing.T) {
	full := ContentHash("up", "", "", "", "0123456789abcdef", nil, "", "")
	short := ContentHash("up", "", "", "", "0123456789abcde", nil, "", "")
	if full == short {
		t.Error("predecessor hash length is not covered")
	}
}

// TestContentHashMatches_AcceptsADigestMintedBeforeExtensionBinding — a
// migration stored months ago must still verify.
//
// T2-6 pass #10, B10-2 (measured). `e1ce9863e` bound the required-extension
// set into `content_sha256`, reasoning that writing the segment "only when
// non-empty" left every earlier digest untouched. That holds for a migration
// declaring NO extensions and fails for exactly the ones that do: those were
// stored — the manifest has travelled since April, `required_extensions`
// since June — hashed WITHOUT the segment. A client carrying the new formula
// recomputes them differently, and `WriteMigration` refuses the fetch with a
// message about hand-editing, which bricks apply including a fresh
// environment's first bootstrap.
//
// The commit's evidence was "verified against the production console: its
// one stored migration carries no manifest" — an existence check over ONE
// deployment rather than over the shape.
func TestContentHashMatches_AcceptsADigestMintedBeforeExtensionBinding(t *testing.T) {
	const (
		up       = "CREATE TABLE t (id uuid primary key);"
		manifest = `{"required_extensions":["pgcrypto","uuid-ossp"]}`
	)

	// What the old compiler minted: the same content, hashed with no
	// knowledge of required extensions.
	legacy := ContentHash(up, "", "", "", "", nil, "", "")
	current := ContentHash(up, "", "", "", "", nil, "", manifest)

	if legacy == current {
		t.Fatal("the two formulas agree, so this test proves nothing — the extension segment stopped affecting the digest")
	}

	// The stored artifact carries the LEGACY digest and the manifest.
	if !ContentHashMatches(up, "", "", "", "", nil, "", manifest, legacy) {
		t.Error("a migration stored before required-extension binding no longer verifies — every fetch of it fails as if it had been tampered with, and apply cannot proceed")
	}
	// And one minted today verifies too.
	if !ContentHashMatches(up, "", "", "", "", nil, "", manifest, current) {
		t.Error("a digest minted under the current formula does not verify")
	}

	// The point of binding the extensions is that STRIPPING them must not
	// pass. A current-formula digest with the list removed has to fail, or
	// the segment buys nothing.
	if ContentHashMatches(up, "", "", "", "", nil, "", "", current) {
		t.Error("stripping the extension list from an artifact minted under the current formula still verifies — that licenses a run the preflight would refuse, which is the whole reason the segment exists")
	}

	// And genuine tampering still fails under both formulas.
	if ContentHashMatches(up+" DROP TABLE t;", "", "", "", "", nil, "", manifest, legacy) {
		t.Error("edited SQL verified against the legacy digest — the fallback must pin every executable segment exactly as the old formula did")
	}
}

// TestMismatchRefusals_DoNotDiagnoseTamperingAsTheOnlyCause — T2-6 pass
// #10, B10-3.
//
// Both refusals attributed every digest mismatch to a hand edit. Since
// required-extension binding there is a second, blameless cause on the
// load side: an artifact fetched by a build whose hash formula differs
// from this one. B10-2 made the CHECK accept both formulas; this pins the
// TEXT, because an operator staring at "someone hand-edited this" for a
// file nobody touched goes looking for an intruder instead of re-fetching.
//
// The two sites need OPPOSITE advice, which is the whole reason this is
// asserted rather than assumed:
//
//   - load side reads artifacts off disk that some earlier fetch wrote, so
//     "re-fetch first" is the cheap discriminator;
//   - WriteMigration is itself the fetch, so re-fetching is exactly what
//     just happened and cannot be the remedy.
func TestMismatchRefusals_DoNotDiagnoseTamperingAsTheOnlyCause(t *testing.T) {
	src, err := readSource("orchestrator.go")
	if err != nil {
		t.Fatalf("read orchestrator.go: %v", err)
	}
	load := messageFor(t, src, `return nil, fmt.Errorf("artifact %s: content_sha256 mismatch`)
	if !containsAll(load, "Re-fetch", "predates required-extension binding") {
		t.Errorf("the load-side refusal does not name the blameless cause or its remedy:\n%s", load)
	}
	write := messageFor(t, src, `return fmt.Errorf("WriteMigration: content_sha256 mismatch`)
	if containsAll(write, "Re-fetch") {
		t.Errorf("the fetch-side refusal advises re-fetching, which is the operation that just produced the mismatch:\n%s", write)
	}
	if !containsAll(write, "console-side mutation") {
		t.Errorf("the fetch-side refusal does not say what a mismatch there actually means:\n%s", write)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// messageFor returns the single source line carrying the refusal that
// starts with `prefix`, so an assertion about one site cannot be satisfied
// by the other's text.
func messageFor(t *testing.T, src, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, prefix) {
			return line
		}
	}
	t.Fatalf("no refusal matching %q in the source", prefix)
	return ""
}

func readSource(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}
