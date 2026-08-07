package txregistry_test

import (
	"context"
	"database/sql"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// T2-6 D-F3 — a method that ADOPTS a caller's transaction does not commit
// it; the orchestrator does, later. Announcing the write when such a
// method returns (an eventbus emit) publishes an event for a mutation
// that is still provisional — so a rollback leaves subscribers acting on
// something that never happened. OnCommit parks the announcement until
// the transaction is actually durable.

func openReg(t *testing.T) (*txregistry.Memory, string) {
	t.Helper()
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return reg, id
}

func TestOnCommit_RunsAfterCommit(t *testing.T) {
	reg, id := openReg(t)
	var order []string
	if !reg.OnCommit(id, func() { order = append(order, "first") }) {
		t.Fatal("OnCommit should resolve a live tx id")
	}
	if !reg.OnCommit(id, func() { order = append(order, "second") }) {
		t.Fatal("second OnCommit should also register")
	}
	if len(order) != 0 {
		t.Fatalf("callbacks must not run before Commit; ran %v", order)
	}
	if err := reg.Commit(context.Background(), id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("callbacks = %v, want [first second] in registration order", order)
	}
}

// The whole point: a rolled-back transaction must announce nothing.
//
// The invariant is about the TRANSACTION, not about one ordering of the two
// goroutines that race over it. Both orderings are reachable: the drain (a
// sibling RPC's grpcrollback, the Tier-2 timeout watcher, the coordinator's
// Rollback RPC) can land after the emit registered, or between the handler's
// last statement and the emit wrapper — see T3-7 pass #7 C-F1/B-F5.
func TestOnCommit_DroppedOnRollback(t *testing.T) {
	t.Run("rollback lands after the emit registered", func(t *testing.T) {
		reg, id := openReg(t)
		fired := false
		reg.OnCommit(id, func() { fired = true })
		if err := reg.Rollback(context.Background(), id); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if fired {
			t.Error("a rolled-back tx must not run its deferred emits — that is the phantom event")
		}
	})

	t.Run("rollback lands before the emit registered", func(t *testing.T) {
		reg, id := openReg(t)
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(txregistry.HeaderName, id))
		dispatched := 0

		// The handler's writes are done and it has returned; the emit
		// wrapper has not registered yet. The transaction is drained in
		// that window.
		if err := reg.Rollback(context.Background(), id); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		emitWrapper(ctx, reg, func() { dispatched++ })

		if dispatched != 0 {
			t.Errorf("a rolled-back tx must not run its deferred emits — that is the phantom event; "+
				"the bus saw %d dispatch(es) for a write that no longer exists", dispatched)
		}
	})
}

// emitWrapper mirrors the branch the eventbus emitter generates into every
// wrapped mutation (srcgo/domains/eventbus/codegen/wrap.go). The invariant
// above is a property of that seam, so the test drives the seam rather than
// the registry call underneath it.
func emitWrapper(ctx context.Context, reg txregistry.CommitHook, emit func()) {
	if txregistry.DeferUntilCommit(ctx, reg, emit) == txregistry.EmitNow {
		emit()
	}
}

// An id the registry doesn't hold reports false, so the caller knows the
// work was NOT parked and has to decide what to do with it —
// [txregistry.DeferUntilCommit] is where that decision lives.
func TestOnCommit_UnknownTxID(t *testing.T) {
	reg, id := openReg(t)
	if err := reg.Commit(context.Background(), id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if reg.OnCommit(id, func() {}) {
		t.Error("a settled tx id must not accept new callbacks")
	}
	if reg.OnCommit("never-existed", func() {}) {
		t.Error("an unknown tx id must report false")
	}
}

func TestDeferUntilCommit(t *testing.T) {
	reg, id := openReg(t)
	ctxWith := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, id))

	fired := false
	if got := txregistry.DeferUntilCommit(ctxWith, reg, func() { fired = true }); got != txregistry.EmitDeferred {
		t.Fatalf("a request carrying a live tx id must defer; got %v", got)
	}
	if fired {
		t.Error("deferred work must not run at registration time")
	}
	if err := reg.Commit(context.Background(), id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !fired {
		t.Error("deferred work must run once the tx commits")
	}

	// No tx on the request — nothing to wait for, so the caller is told
	// to run the work itself. This is the ordinary path: a method that
	// opened its own tx has already committed by the time it returns.
	if got := txregistry.DeferUntilCommit(context.Background(), reg, func() {}); got != txregistry.EmitNow {
		t.Errorf("a request with no tx id must run the work now; got %v", got)
	}
	// A tx id the registry does not hold is NOT the same situation. The
	// request asserted a transaction; the handler could only have got this
	// far by adopting it, so the entry was drained under it. Running the
	// work would announce a write a concurrent rollback discarded.
	ctxStale := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, "stale"))
	if got := txregistry.DeferUntilCommit(ctxStale, reg, func() {}); got != txregistry.EmitDropped {
		t.Errorf("a tx id the registry no longer holds must drop the work; got %v", got)
	}
	// Nil registry / nil fn are misuse, not a panic.
	if got := txregistry.DeferUntilCommit(ctxWith, nil, func() {}); got != txregistry.EmitNow {
		t.Errorf("nil registry must fall back to running the work; got %v", got)
	}
}
