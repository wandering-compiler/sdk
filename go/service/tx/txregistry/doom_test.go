package txregistry_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// A rollback that has to wait for a lease used to be abandoned when the wait
// gave up — the caller's context was done, or the finish-wait ran out — and
// the transaction stayed live and committable. Asking for a rollback is a
// decision; these tests pin that the decision survives the wait.

func countRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The rollback's own context is already done and a sibling still holds the
// transaction: the rollback cannot run now, but the transaction must not be
// committable afterwards.
func TestRollbackBehindALease_CancelledContext_DoomsTheTransaction(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	// A generous finish-wait: only the cancelled context makes the wait end.
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db}, txregistry.WithFinishWait(time.Minute))
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := leaseCtx(id)
	tx, _, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil {
		t.Fatalf("AdoptTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = reg.RollbackCaused(cancelled, id, "a sibling call failed")
	if !errors.Is(err, txregistry.ErrTxDoomed) || !errors.Is(err, txregistry.ErrTxBusy) {
		t.Fatalf("RollbackCaused behind a lease on a done ctx = %v, want ErrTxDoomed wrapping ErrTxBusy", err)
	}
	assertDoomed(t, reg, db, id, tx, release)
}

// The same with a live context and no finish-wait at all.
func TestRollbackBehindALease_ZeroFinishWait_DoomsTheTransaction(t *testing.T) {
	reg, db, id := leaseReg(t) // WithFinishWait(0)
	ctx := leaseCtx(id)
	tx, _, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil {
		t.Fatalf("AdoptTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Rollback(context.Background(), id); !errors.Is(err, txregistry.ErrTxDoomed) {
		t.Fatalf("Rollback behind a lease with no finish-wait = %v, want ErrTxDoomed", err)
	}
	assertDoomed(t, reg, db, id, tx, release)
}

func assertDoomed(t *testing.T, reg *txregistry.Memory, db *sql.DB, id string, tx *sql.Tx, release txregistry.ReleaseFunc) {
	t.Helper()
	// Commit is refused at once, without waiting for the holder.
	if err := reg.Commit(context.Background(), id); !errors.Is(err, txregistry.ErrTxDoomed) {
		t.Fatalf("Commit of a doomed transaction = %v, want ErrTxDoomed — a transaction "+
			"with a failed participant must not commit", err)
	}
	// No new work joins it.
	if _, _, rel, err := txregistry.AdoptTx(leaseCtx(id), reg, "main"); !errors.Is(err, txregistry.ErrTxDoomed) {
		rel()
		t.Fatalf("AdoptTx of a doomed transaction = %v, want ErrTxDoomed", err)
	}
	// The holder is not cut off mid-method.
	if _, err := tx.ExecContext(context.Background(), "INSERT INTO t (id) VALUES (2)"); err != nil {
		t.Fatalf("the running holder's next statement must still run: %v", err)
	}
	if reg.Active() != 1 {
		t.Fatalf("Active() = %d before the holder released, want 1", reg.Active())
	}
	// Its release runs the deferred rollback.
	release()
	if reg.Active() != 0 {
		t.Fatalf("Active() = %d after the holder released, want 0 — the deferred rollback did not run", reg.Active())
	}
	if n := countRows(t, db); n != 0 {
		t.Fatalf("%d rows survived a doomed transaction, want 0", n)
	}
	err := reg.Commit(context.Background(), id)
	if !errors.Is(err, txregistry.ErrUnknownTxID) || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("Commit after the deferred rollback = %v, want ErrUnknownTxID naming the rollback", err)
	}
}

// A Commit already waiting for the lease when the rollback is asked for must
// not win the race and commit: whoever finishes a doomed entry rolls it back.
func TestDoom_ACommitQueuedBeforeTheRollbackDoesNotCommit(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db}, txregistry.WithFinishWait(time.Minute))
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := leaseCtx(id)
	tx, _, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}

	commitErr := make(chan error, 1)
	go func() { commitErr <- reg.Commit(context.Background(), id) }()
	// Let the Commit park behind the lease.
	deadline := time.Now().Add(5 * time.Second)
	for {
		// A short probe: until the Commit queues, the probe waits on the
		// lease and times out; once it has, it is refused at once.
		probe, stop := context.WithTimeout(leaseCtx(id), 5*time.Millisecond)
		_, _, rel, err := txregistry.AdoptTx(probe, reg, "main")
		stop()
		rel()
		if errors.Is(err, txregistry.ErrTxBusy) && strings.Contains(err.Error(), "being committed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the Commit never queued behind the lease")
		}
		time.Sleep(time.Millisecond)
	}

	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reg.Rollback(done, id); !errors.Is(err, txregistry.ErrTxDoomed) {
		t.Fatalf("Rollback = %v, want ErrTxDoomed", err)
	}
	release()

	if err := <-commitErr; err == nil {
		t.Fatal("the queued Commit committed a doomed transaction")
	}
	if n := countRows(t, db); n != 0 {
		t.Fatalf("%d rows committed, want 0", n)
	}
	if reg.Active() != 0 {
		t.Fatalf("Active() = %d, want 0", reg.Active())
	}
}

// An after-commit callback that panics must not escape Commit: the
// transaction is durable, the remaining callbacks must still run, and the
// panic is reported to the operator.
func TestCommit_APanickingHookIsRecoveredAndTheRestRun(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := leaseCtx(id)
	tx, _, release, err := txregistry.AdoptTx(ctx, reg, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	release()

	var ran []string
	reg.OnCommit(id, func() { ran = append(ran, "first"); panic("emit exploded") })
	reg.OnCommit(id, func() { ran = append(ran, "second") })

	var commitErr error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("a hook's panic escaped Commit: %v", p)
			}
		}()
		commitErr = reg.Commit(context.Background(), id)
	}()
	if commitErr != nil {
		t.Fatalf("Commit = %v for a transaction that committed", commitErr)
	}
	if len(ran) != 2 || ran[1] != "second" {
		t.Fatalf("hooks ran %v, want [first second] — a panic must not skip the rest", ran)
	}
	if n := countRows(t, db); n != 1 {
		t.Fatalf("rows = %d, want 1 (the commit is durable)", n)
	}
	if !strings.Contains(logged.String(), "emit exploded") {
		t.Fatalf("the hook's panic was not reported; log = %q", logged.String())
	}
}
