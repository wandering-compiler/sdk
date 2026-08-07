package txregistry_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// An emit must never be published for a write that was rolled back.
//
// DeferUntilCommit answers one question — "may I announce this write now?" —
// and it used to answer it with a bool that meant two different things.
// `false` covered both "this request carries no transaction" (announce away:
// the method committed its own write before returning) and "it carries one,
// but the registry no longer holds it". The second is not the same situation
// at all: the transaction is GONE, drained by a rollback, an orphan timeout,
// or a sibling RPC's failure on the same shared transaction — and the write
// this emit describes has been undone.
//
// The generated emit wrapper treats `false` as "no transaction, emit now", so
// the ambiguity published a phantom event: a real broker Dispatch, delivered
// to real subscribers, describing a mutation that never landed. There is no
// retraction anywhere downstream.
//
// The window is a two-goroutine interleave — the RPC's own goroutine between
// its write and this call, against whichever goroutine drains the tx — so it
// cannot be reached sequentially, and no test that drives one request at a
// time will ever see it.
func TestDeferUntilCommit_DropsTheEmitWhenTheTxWasDrainedUnderIt(t *testing.T) {
	reg, id := openReg(t)
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, id))

	// The drainer wins: the transaction this request adopted is rolled back
	// while the request is still in flight.
	if err := reg.Rollback(context.Background(), id); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	fired := false
	handled := txregistry.DeferUntilCommit(ctx, reg, func() { fired = true })

	if fired {
		t.Error("the emit ran for a write that was rolled back — subscribers are now acting " +
			"on a mutation that never happened, and nothing downstream can retract it")
	}
	if handled != txregistry.EmitDropped {
		t.Errorf("DeferUntilCommit answered %v for a request that DOES carry a transaction id "+
			"whose tx was drained under it; want EmitDropped. EmitNow is the answer the caller "+
			"reads as 'announce it', which is exactly what must not happen here", handled)
	}
}

// The permissive half of the same statement, kept next to it: a request with
// no transaction at all still emits immediately. Failing closed on the
// ambiguous case must not turn into failing closed on the ordinary one — that
// would silence every event in every method that opens its own transaction,
// which is most of them.
func TestDeferUntilCommit_StillEmitsImmediatelyWithoutATransaction(t *testing.T) {
	reg, _ := openReg(t)

	if got := txregistry.DeferUntilCommit(context.Background(), reg, func() {}); got != txregistry.EmitNow {
		t.Errorf("a request with no tx id must emit now, not defer; got %v", got)
	}
}
