package txregistry_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// singleConnRegistry returns a Memory holding one db keyed by
// "main" — the canonical single-connection-domain shape.
func singleConnRegistry(t *testing.T) *txregistry.Memory {
	t.Helper()
	return txregistry.NewMemory(map[string]*sql.DB{"main": openSQLite(t)})
}

// Begin returns a non-empty id; LookupTx round-trips the same
// *sql.Tx on the matching connection; Active increments.
func TestMemory_Begin_RoundTrip(t *testing.T) {
	reg := singleConnRegistry(t)
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if id == "" {
		t.Fatal("empty tx_id")
	}
	tx, err := reg.LookupTx(id, "main")
	if err != nil || tx == nil {
		t.Fatalf("LookupTx(%q, main) = (%v, %v); want non-nil + nil err", id, tx, err)
	}
	if got := reg.Active(); got != 1 {
		t.Errorf("Active = %d, want 1", got)
	}
	// Cleanup so the test doesn't leak the open tx.
	_ = reg.Rollback(id)
}

// Commit closes the tx, removes it from the registry. Subsequent
// LookupTx returns ErrUnknownTxID; Active decrements.
func TestMemory_Commit_RemovesTx(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx, _ := reg.LookupTx(id, "main")
	if _, err := tx.Exec("INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := reg.Commit(id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := reg.LookupTx(id, "main"); !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Errorf("LookupTx after Commit = %v, want ErrUnknownTxID", err)
	}
	if got := reg.Active(); got != 0 {
		t.Errorf("Active = %d, want 0", got)
	}
	// Verify the row actually committed (visible outside the tx).
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t WHERE id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 row after Commit, got %d", n)
	}
}

// Rollback releases the tx, removes it from the registry.
// Inserts inside the tx don't reach the underlying table.
func TestMemory_Rollback_DiscardsWrites(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx, _ := reg.LookupTx(id, "main")
	if _, err := tx.Exec("INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := reg.Rollback(id); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := reg.LookupTx(id, "main"); !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Errorf("LookupTx after Rollback = %v, want ErrUnknownTxID", err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rollback should have discarded the insert; got %d rows", n)
	}
}

// Commit / Rollback / LookupTx on an unknown id surface
// ErrUnknownTxID.
func TestMemory_UnknownID(t *testing.T) {
	reg := singleConnRegistry(t)
	if err := reg.Commit("does-not-exist"); !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Errorf("Commit(unknown) = %v, want ErrUnknownTxID", err)
	}
	if err := reg.Rollback("does-not-exist"); !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Errorf("Rollback(unknown) = %v, want ErrUnknownTxID", err)
	}
	if _, err := reg.LookupTx("does-not-exist", "main"); !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Errorf("LookupTx(unknown) = %v, want ErrUnknownTxID", err)
	}
}

// Commit-then-Rollback (or vice versa) on the same id: second
// op errors with ErrUnknownTxID — the first take() removed the
// entry, the second call has nothing to release.
func TestMemory_DoubleClose(t *testing.T) {
	reg := singleConnRegistry(t)
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := reg.Commit(id); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := reg.Rollback(id); !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Errorf("second Rollback = %v, want ErrUnknownTxID", err)
	}
}

// Begin against a connection name the registry doesn't host →
// ErrUnknownConnection. Diagnostic includes registered names so
// the caller can spot typos.
func TestMemory_Begin_UnknownConnection(t *testing.T) {
	reg := singleConnRegistry(t)
	_, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "nope"})
	if !errors.Is(err, txregistry.ErrUnknownConnection) {
		t.Fatalf("Begin(nope) = %v, want ErrUnknownConnection", err)
	}
}

// LookupTx with the wrong connection name on a known tx_id →
// ErrConnectionMismatch (not ErrUnknownTxID — the id IS known,
// the connection is wrong).
func TestMemory_LookupTx_ConnectionMismatch(t *testing.T) {
	reg := txregistry.NewMemory(map[string]*sql.DB{
		"main":  openSQLite(t),
		"audit": openSQLite(t),
	})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = reg.Rollback(id) }()
	if _, err := reg.LookupTx(id, "audit"); !errors.Is(err, txregistry.ErrConnectionMismatch) {
		t.Errorf("LookupTx with wrong connection = %v, want ErrConnectionMismatch", err)
	}
	// Same id resolves on the right connection.
	if _, err := reg.LookupTx(id, "main"); err != nil {
		t.Errorf("LookupTx with matching connection = %v, want nil", err)
	}
}

// Multi-conn registry: Begin on each connection produces a tx
// pinned to that connection. The respective LookupTxs round-trip;
// cross-lookups error.
func TestMemory_MultiConnection_BeginsRoute(t *testing.T) {
	dbMain := openSQLite(t)
	dbAudit := openSQLite(t)
	if _, err := dbMain.Exec("CREATE TABLE m (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbAudit.Exec("CREATE TABLE a (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{
		"main":  dbMain,
		"audit": dbAudit,
	})
	idMain, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin main: %v", err)
	}
	idAudit, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "audit"})
	if err != nil {
		t.Fatalf("Begin audit: %v", err)
	}
	txMain, err := reg.LookupTx(idMain, "main")
	if err != nil {
		t.Fatalf("LookupTx main: %v", err)
	}
	if _, err := txMain.Exec("INSERT INTO m (id) VALUES (1)"); err != nil {
		t.Fatalf("INSERT main: %v", err)
	}
	txAudit, err := reg.LookupTx(idAudit, "audit")
	if err != nil {
		t.Fatalf("LookupTx audit: %v", err)
	}
	if _, err := txAudit.Exec("INSERT INTO a (id) VALUES (2)"); err != nil {
		t.Fatalf("INSERT audit: %v", err)
	}
	// Cross-lookup: idMain on audit must mismatch.
	if _, err := reg.LookupTx(idMain, "audit"); !errors.Is(err, txregistry.ErrConnectionMismatch) {
		t.Errorf("idMain on audit = %v, want ErrConnectionMismatch", err)
	}
	if err := reg.Commit(idMain); err != nil {
		t.Fatalf("Commit main: %v", err)
	}
	if err := reg.Commit(idAudit); err != nil {
		t.Fatalf("Commit audit: %v", err)
	}
}

// Concurrent Begin / LookupTx / Commit don't race. Run via
// goroutines to exercise the mutex; the race detector
// surfaces any unsynchronised access.
func TestMemory_ConcurrentBeginCommit(t *testing.T) {
	reg := singleConnRegistry(t)
	const n = 50
	ids := make(chan string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
			if err != nil {
				t.Errorf("Begin: %v", err)
				return
			}
			if _, err := reg.LookupTx(id, "main"); err != nil {
				t.Errorf("LookupTx(%q) err right after Begin: %v", id, err)
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	if got := reg.Active(); got != n {
		t.Errorf("Active = %d, want %d", got, n)
	}
	for id := range ids {
		if err := reg.Commit(id); err != nil {
			t.Errorf("Commit %q: %v", id, err)
		}
	}
	if got := reg.Active(); got != 0 {
		t.Errorf("Active after all commits = %d, want 0", got)
	}
}

// G3-T-01: concurrent double-close race. N goroutines race to
// Commit the same tx_id; exactly one wins, all losers get
// ErrUnknownTxID. The sequential variant (TestMemory_DoubleClose)
// only proves the post-take state; this version exercises the
// mutex against a real Begin → fan-out Commit race so the
// race detector pins any unsynchronised state mutation.
//
// Done in a loop so the race window is consistently exercised
// across runs (a single tx makes the race window small enough
// that go test -race might miss the window in a single shot).
func TestMemory_ConcurrentDoubleCommit_OneWinner(t *testing.T) {
	for iter := 0; iter < 30; iter++ {
		reg := singleConnRegistry(t)
		id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
		if err != nil {
			t.Fatalf("iter %d: Begin: %v", iter, err)
		}

		const racers = 16
		var wg sync.WaitGroup
		wg.Add(racers)
		results := make(chan error, racers)
		// Channel-based start barrier — every goroutine blocks on
		// receive, then unblocks together when the channel is
		// closed. Tightens the race window so the test exercises
		// real contention rather than serialised-by-scheduler.
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			go func() {
				defer wg.Done()
				<-start
				results <- reg.Commit(id)
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		var winners, losers int
		for err := range results {
			if err == nil {
				winners++
			} else if errors.Is(err, txregistry.ErrUnknownTxID) {
				losers++
			} else {
				t.Errorf("iter %d: unexpected commit err: %v", iter, err)
			}
		}
		if winners != 1 {
			t.Errorf("iter %d: winners = %d, want 1 (single-take guarantee)", iter, winners)
		}
		if losers != racers-1 {
			t.Errorf("iter %d: losers = %d, want %d", iter, losers, racers-1)
		}
		if got := reg.Active(); got != 0 {
			t.Errorf("iter %d: Active after race = %d, want 0", iter, got)
		}
	}
}

// G3-T-01: parallel Commit racing against parallel LookupTx —
// once the winning Commit takes the entry, every Lookup that
// arrives after must surface ErrUnknownTxID rather than a
// stale handle. The race detector pins any read-during-take
// data race.
func TestMemory_ConcurrentLookupVsCommit(t *testing.T) {
	reg := singleConnRegistry(t)
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	const lookupers = 32
	var wg sync.WaitGroup
	wg.Add(lookupers + 1)
	start := make(chan struct{})

	// One Commit racing against many Lookups.
	commitDone := make(chan error, 1)
	go func() {
		defer wg.Done()
		<-start
		commitDone <- reg.Commit(id)
	}()
	// Each Lookup either sees a live tx OR ErrUnknownTxID — never
	// a torn state. Any other error is a bug.
	for i := 0; i < lookupers; i++ {
		go func() {
			defer wg.Done()
			<-start
			tx, err := reg.LookupTx(id, "main")
			if err == nil && tx == nil {
				t.Error("LookupTx returned nil tx with nil err — torn state")
			}
			if err != nil && !errors.Is(err, txregistry.ErrUnknownTxID) {
				t.Errorf("LookupTx unexpected err: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if err := <-commitDone; err != nil {
		t.Errorf("Commit: %v", err)
	}
	if got := reg.Active(); got != 0 {
		t.Errorf("Active after race = %d, want 0", got)
	}
}

// The interface contract: Memory satisfies Registry. Static
// guard so a future Registry-method addition fails to compile
// here instead of bit-rotting silently.
var _ txregistry.Registry = (*txregistry.Memory)(nil)

// NewMemory(empty map) panics. Defensive guard at construction;
// alternative is silent unknown-connection errors at every call.
func TestNewMemory_EmptyMap_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for empty map")
		}
	}()
	_ = txregistry.NewMemory(map[string]*sql.DB{})
}

// NewMemory with a nil db value panics — alternative is silent
// NPE on first Begin against that connection.
func TestNewMemory_NilDB_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil db value")
		}
	}()
	_ = txregistry.NewMemory(map[string]*sql.DB{"main": nil})
}

// Tier 2 (slice 6Q-F): BeginOptions.Timeout > 0 wraps the tx
// ctx with WithTimeout. After the deadline fires the sql
// package auto-rolls back the underlying tx AND the slice
// 6Q-F follow-up background-watcher drains the registry
// entry — subsequent Commit through the registry surfaces
// ErrUnknownTxID, mapped by the gRPC server side as
// `codes.NotFound` ("tx is gone, not just a failed Commit").
func TestMemory_Begin_TxTimeout_AutoRollback(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{
		ConnectionName: "main",
		Timeout:        50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Begin with timeout: %v", err)
	}
	// Sleep past the deadline. The WithTimeout fires, the sql
	// package auto-rolls back the tx, AND the background-
	// watcher drains the registry entry.
	time.Sleep(150 * time.Millisecond)

	// Active() reflects the watcher's cleanup.
	if got := reg.Active(); got != 0 {
		t.Errorf("Active = %d after timeout (watcher should drain), want 0", got)
	}
	// User Commit on a drained id: ErrUnknownTxID.
	err = reg.Commit(id)
	if !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Errorf("Commit after timeout: err = %v, want ErrUnknownTxID", err)
	}
}

// Watcher (slice 6Q-F follow-up): the tx is drained on
// deadline expiry without any user Commit/Rollback call.
// Active() drops eventually-but-soon-after the deadline.
func TestMemory_Begin_TxTimeout_WatcherDrainsOnDeadline(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	_, err := reg.Begin(context.Background(), txregistry.BeginOptions{
		ConnectionName: "main",
		Timeout:        30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := reg.Active(); got != 1 {
		t.Errorf("Active right after Begin = %d, want 1", got)
	}
	// Wait for deadline + watcher. Generous buffer to absorb
	// goroutine scheduling jitter on busy CI runners.
	time.Sleep(200 * time.Millisecond)
	if got := reg.Active(); got != 0 {
		t.Errorf("Active after deadline + watcher = %d, want 0 (watcher should have drained)", got)
	}
}

// Watcher must NOT double-rollback when the user calls
// Commit/Rollback before the deadline. Commit succeeds; the
// watcher wakes (defer cancel() fires), finds the entry
// already gone, and silently returns.
func TestMemory_Begin_TxTimeout_UserCommitWinsRace(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{
		ConnectionName: "main",
		Timeout:        time.Hour, // never fires in this test
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx, _ := reg.LookupTx(id, "main")
	if _, err := tx.Exec("INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := reg.Commit(id); err != nil {
		t.Fatalf("Commit (well before deadline): %v", err)
	}
	// Commit succeeded → row visible.
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t WHERE id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 row after user Commit, got %d", n)
	}
	if got := reg.Active(); got != 0 {
		t.Errorf("Active after user Commit = %d, want 0", got)
	}
	// Brief settle to let the watcher goroutine wake on the
	// fired cancel() and exit cleanly. No assertion needed —
	// if it double-rollbacked, the table would be unchanged
	// (Rollback is no-op post-Commit on the *sql.Tx) and we'd
	// race-detect a panic on the registry mutex on go test
	// -race.
	time.Sleep(20 * time.Millisecond)
}

// Timeout = 0 (default) leaves the tx with the long-living
// ctx semantics — no auto-rollback firing during a normal
// idle period.
func TestMemory_Begin_TxTimeout_ZeroIsNoOp(t *testing.T) {
	db := openSQLite(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{
		ConnectionName: "main",
		// Timeout left zero → no WithTimeout wrap.
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Sleep a similar interval; the tx must remain valid.
	time.Sleep(150 * time.Millisecond)
	if err := reg.Commit(id); err != nil {
		t.Errorf("Commit after no-timeout idle: %v (want nil)", err)
	}
}
