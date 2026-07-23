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
	if err := reg.Commit(id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("callbacks = %v, want [first second] in registration order", order)
	}
}

// The whole point: a rolled-back transaction must announce nothing.
func TestOnCommit_DroppedOnRollback(t *testing.T) {
	reg, id := openReg(t)
	fired := false
	reg.OnCommit(id, func() { fired = true })
	if err := reg.Rollback(id); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if fired {
		t.Error("a rolled-back tx must not run its deferred emits — that is the phantom event")
	}
}

// An id the registry doesn't hold reports false, so the caller knows to
// run the work itself rather than silently dropping it.
func TestOnCommit_UnknownTxID(t *testing.T) {
	reg, id := openReg(t)
	if err := reg.Commit(id); err != nil {
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
	if !txregistry.DeferUntilCommit(ctxWith, reg, func() { fired = true }) {
		t.Fatal("a request carrying a live tx id must defer")
	}
	if fired {
		t.Error("deferred work must not run at registration time")
	}
	if err := reg.Commit(id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !fired {
		t.Error("deferred work must run once the tx commits")
	}

	// No tx on the request — nothing to wait for, so the caller is told
	// to run the work itself. This is the ordinary path: a method that
	// opened its own tx has already committed by the time it returns.
	if txregistry.DeferUntilCommit(context.Background(), reg, func() {}) {
		t.Error("a request with no tx id must not defer")
	}
	// A tx id the registry never held behaves the same way.
	ctxStale := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, "stale"))
	if txregistry.DeferUntilCommit(ctxStale, reg, func() {}) {
		t.Error("a stale tx id must not defer")
	}
	// Nil registry / nil fn are misuse, not a panic.
	if txregistry.DeferUntilCommit(ctxWith, nil, func() {}) {
		t.Error("nil registry must report false")
	}
}
