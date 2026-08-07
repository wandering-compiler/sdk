package txregistry_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// T3-7 pass #7 C-F2 / C-F3 — the registry used to hand the same *sql.Tx to
// anyone who asked and let a finisher close it under a running handler. Both
// are one missing concept: an ADOPTION LEASE. These tests state the
// invariants the lease buys, as invariants — none of them depends on the
// race detector, and none on a scheduler hint.

func leaseCtx(txID string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, txID))
}

// leaseReg builds a registry whose waits give up immediately, so the
// "cannot get in" direction is a deterministic error rather than a delay.
func leaseReg(t *testing.T) (*txregistry.Memory, *sql.DB, string) {
	t.Helper()
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db}, txregistry.WithFinishWait(0))
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return reg, db, id
}

// C-F2 — a method's writes are all-or-nothing. A finisher that lands between
// two statements of an adopted handler commits statement 1 and fails
// statement 2, so the caller is told the method failed while half of it is
// durable.
func TestAdoptedHandler_FinisherCannotSplitAMethod(t *testing.T) {
	reg, db, id := leaseReg(t)
	ctx := leaseCtx(id)

	tx, adopted, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil || !adopted {
		t.Fatalf("AdoptTx: tx=%v adopted=%v err=%v", tx, adopted, err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatalf("statement 1: %v", err)
	}

	// The coordinator commits while the handler is between its statements
	// (its client-side deadline expired, a retry framework fired, or plain
	// orchestration bug). It must not be able to close a transaction a
	// handler is still using.
	if err := reg.Commit(context.Background(), id); !errors.Is(err, txregistry.ErrTxBusy) {
		t.Fatalf("Commit while an adopted handler holds the tx = %v, want ErrTxBusy", err)
	}

	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (2)"); err != nil {
		t.Fatalf("statement 2 of an adopted handler must still run — a finisher "+
			"must not close the tx mid-method (half the method is now durable): %v", err)
	}

	// The refusal left the transaction exactly as it was: still open, still
	// the coordinator's to finish once the handler is done.
	release()
	if err := reg.Commit(context.Background(), id); err != nil {
		t.Fatalf("Commit after the handler released: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("committed rows = %d, want 2 — both statements of the method", n)
	}
}

// The finisher WAITS rather than refusing on sight: an orchestrator that
// commits the moment its last RPC returns must not race the handler's own
// unwind.
func TestFinisher_WaitsForTheAdopterToRelease(t *testing.T) {
	reg, id := openReg(t) // default finish wait
	ctx := leaseCtx(id)

	tx, adopted, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil || !adopted {
		t.Fatalf("AdoptTx: %v / %v", adopted, err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatalf("statement 1: %v", err)
	}

	committed := make(chan error, 1)
	go func() { committed <- reg.Commit(context.Background(), id) }()

	select {
	case err := <-committed:
		t.Fatalf("Commit finished while the handler still held the tx (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
		// Still parked, as it must be.
	}

	release()
	select {
	case err := <-committed:
		if err != nil {
			t.Fatalf("Commit after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Commit never resumed after the lease was released")
	}
}

// C-F3 — one transaction, one adopter at a time. Two RPCs holding the same
// *sql.Tx interleave on a single backend connection; an open *sql.Rows from
// a locked query plus a concurrent mutation desyncs the driver protocol.
func TestAdoptedHandler_SecondAdopterIsNotHandedTheSameTx(t *testing.T) {
	reg, _, id := leaseReg(t)
	ctx := leaseCtx(id)

	first, adopted, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil || !adopted {
		t.Fatalf("first AdoptTx: %v / %v", adopted, err)
	}

	second, adopted2, release2, err := txregistry.AdoptTx(ctx, reg, "main")
	if err == nil && adopted2 && second != nil {
		t.Fatal("a second concurrent adopter was handed the same *sql.Tx — " +
			"nothing serialises two RPCs on one backend connection")
	}
	if !errors.Is(err, txregistry.ErrTxBusy) {
		t.Fatalf("second AdoptTx = %v, want ErrTxBusy", err)
	}
	release2() // the no-op handed out alongside an error must be safe

	// Serialised, not refused forever: once the first handler releases, the
	// next RPC of the same transaction adopts normally.
	release()
	again, adopted3, release3, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil || !adopted3 || again != first {
		t.Fatalf("adoption after release: tx-identical=%v adopted=%v err=%v", again == first, adopted3, err)
	}
	release3()
}

// A second adopter that arrives while the first is running WAITS for it,
// rather than failing the RPC — the fan-out shape that worked before the
// lease keeps working, it is just serialised now.
func TestSecondAdopter_WaitsForTheFirst(t *testing.T) {
	reg, id := openReg(t) // default finish wait
	ctx := leaseCtx(id)

	_, adopted, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil || !adopted {
		t.Fatalf("first AdoptTx: %v / %v", adopted, err)
	}

	type result struct {
		ok  bool
		err error
	}
	got := make(chan result, 1)
	go func() {
		_, ok, rel, err := txregistry.AdoptTx(ctx, reg, "main")
		rel()
		got <- result{ok, err}
	}()

	select {
	case r := <-got:
		t.Fatalf("second adopter got in while the first held the tx (ok=%v err=%v)", r.ok, r.err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case r := <-got:
		if !r.ok || r.err != nil {
			t.Fatalf("second adopter after release: ok=%v err=%v", r.ok, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second adopter never woke after the lease was released")
	}
}

// A lease must not outlive the Tier-2 orphan watcher: that watcher is the
// reclaimer of last resort, so it drains the entry even under a lease (the
// tx is already rolled back by then) and wakes whoever was waiting on it.
func TestTimeoutWatcher_DrainsUnderALease(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{
		ConnectionName: "main",
		Timeout:        20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ctx := leaseCtx(id)
	_, adopted, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil || !adopted {
		t.Fatalf("AdoptTx: %v / %v", adopted, err)
	}
	defer release()

	deadline := time.Now().Add(5 * time.Second)
	for reg.Active() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the Tier-2 watcher never drained an entry held under a lease — " +
				"a wedged handler would pin the tx and its pooled connection forever")
		}
		time.Sleep(time.Millisecond)
	}
}
