package txregistry_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// A tier with no registry (the business tier) reached over the wire with a
// caller's `w17-tx-id` cannot wait for that transaction's commit: it is in
// another process, and nothing here hears of its outcome. The announcement
// still goes out at once, as it always did — but that is an event published
// before its transaction committed, kept if it rolls back, so it is said
// rather than done in silence.
func TestDeferUntilCommit_ARemoteTransactionItCannotWaitForIsSaid(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(txregistry.HeaderName, "tx-remote-1"))
	if got := txregistry.DeferUntilCommit(ctx, nil, func() {}); got != txregistry.EmitNow {
		t.Fatalf("outcome %v, want EmitNow", got)
	}
	if !strings.Contains(buf.String(), "tx-remote-1") || !strings.Contains(buf.String(), "before that transaction commits") {
		t.Errorf("an announcement inside a transaction this process cannot wait for went out unsaid:\n%s", buf.String())
	}

	// No transaction on the request: nothing to say.
	buf.Reset()
	_ = txregistry.DeferUntilCommit(context.Background(), nil, func() {})
	if strings.Contains(buf.String(), "before that transaction commits") {
		t.Errorf("a request with no transaction was reported:\n%s", buf.String())
	}
}
