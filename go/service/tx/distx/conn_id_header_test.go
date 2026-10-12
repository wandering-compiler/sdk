package distx_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/service/tx/distx"
)

// The header's VALUE is a cross-package contract: `lib/eventbus` strips it
// from every event envelope as a literal (reservedMetadataKeys in
// eventctx.go), for the same reason and under the same import constraint as
// the transaction id (see txregistry.TestHeaderName_IsTheStrippedEnvelopeKey).
func TestConnIDHeader_IsTheStrippedEnvelopeKey(t *testing.T) {
	if distx.ConnIDHeader != "w17-conn-id" {
		t.Fatalf("ConnIDHeader = %q: lib/eventbus strips %q from event envelopes as a literal "+
			"(reservedMetadataKeys in eventctx.go) — update it in the same commit, or "+
			"subscribers will replay a stale routing token onto their storage calls",
			distx.ConnIDHeader, "w17-conn-id")
	}
}
