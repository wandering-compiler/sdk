package txregistry_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// The header's VALUE is a cross-package contract, so it is pinned here.
//
// `lib/eventbus` must strip this header from every event envelope: an emit is
// deferred until commit, so a transaction id replayed into a subscriber names
// a transaction that is already settled, and the storage tier refuses it —
// silently, into a DLQ. eventbus cannot import this package (lib/ does not
// depend on service/), and this package must not import eventbus either: a
// test-only import here would drag NATS and its dependencies into the go.sum
// of every downstream module, because `go mod tidy` keeps sums sufficient to
// test dependencies. Plugin modules would inherit the whole broker stack from
// an assertion.
//
// So both ends pin the same literal, and renaming this one fails here with a
// pointer at the other.
func TestHeaderName_IsTheStrippedEnvelopeKey(t *testing.T) {
	if txregistry.HeaderName != "w17-tx-id" {
		t.Fatalf("HeaderName = %q: lib/eventbus strips %q from event envelopes as a literal "+
			"(reservedMetadataKeys in eventctx.go) — update it in the same commit, or events "+
			"emitted inside a transaction will start failing delivery in silence",
			txregistry.HeaderName, "w17-tx-id")
	}
}
