package txregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wandering-compiler/sdk/go/core/observx"
)

// Memory is the single-instance in-memory implementation of
// [Registry] — the W17DistributedTransaction backing the
// `tx_id → *sql.Tx` map for one storage binary. Per
// `docs/archive/iteration-2-dql.md` D-iter2-dql-11, this is the
// default deployment shape for small-to-mid-sized projects:
// one storage binary per domain → no cross-binary `conn_id`
// routing axis required.
//
// Per `docs/archive/iteration-2-multidb.md` §M2-D, Memory holds one
// `*sql.DB` per declared connection (keyed by connection_name
// from `(w17.module).connection.name`). [Begin] dispatches
// against the named DB; [LookupTx] enforces that adoption only
// succeeds on the same connection the tx was opened on
// (cross-connection adoption is a correctness violation —
// returns [ErrConnectionMismatch]).
//
// **PG / pgbouncer caveat.** PostgreSQL binds a transaction to
// one backend connection for its full lifetime. Operators that
// front their DB with pgbouncer must use **session-pooling**
// mode (or no pgbouncer); transaction-pooling mode releases
// the backend between statements, so the second RPC of a
// `w17-tx-id` flow could resume on a different backend and
// fail. Memory itself doesn't enforce this — it's a
// deployment-shape constraint surfaced in the architecture
// doc, the operator owns the choice.
//
// Multi-instance scale-out lives in a separate Rust grpcproxy
// (parked, see D-iter2-dql-11). Until that lands, deploy
// single-instance OR accept that `tx_id` adoption silently
// falls back to per-method fresh-tx (the slice 6H default).
//
// Memory is safe for concurrent use across goroutines —
// gRPC handlers servicing Begin / Commit / Rollback / storage
// methods all touch the same instance.
type Memory struct {
	dbs map[string]*sql.DB
	mu  sync.Mutex
	txs map[string]txEntry
}

// txEntry records the connection a tx was opened on alongside
// the live *sql.Tx. LookupTx checks the recorded connection
// against the calling method's connection so cross-connection
// adoption can be refused (see [ErrConnectionMismatch]).
//
// `cancel` is non-nil only when [BeginOptions.Timeout] > 0:
// the tx's ctx was wrapped with `context.WithTimeout` and the
// CancelFunc must fire on Commit / Rollback to release the
// timer goroutine (otherwise it lives until the deadline,
// leaking memory across many short-lived txs). When timeout
// fires before Commit / Rollback, the sql package auto-rolls
// back the tx and subsequent ExecContext / Commit return an
// error — the registry entry stays put until the user
// explicitly closes it.
type txEntry struct {
	tx       *sql.Tx
	connName string
	cancel   context.CancelFunc

	// onCommit holds work deferred until this tx actually commits —
	// today, the eventbus emits of every method that ran inside it.
	// A method that ADOPTS a caller's tx does not commit; the caller
	// does, later. Emitting when such a method returns publishes an
	// event for a write that is still provisional, so a rollback
	// leaves subscribers acting on a mutation that never happened.
	// Callbacks run after a successful Commit, and are discarded on
	// Rollback.
	onCommit []func()
}

// BeginOptions drives one Begin call: the connection the tx
// runs on (`ConnectionName`, looked up in Memory's per-connection
// `*sql.DB` map) plus the standard `*sql.TxOptions` (isolation
// level, read-only flag) and an optional [Timeout] for the tx
// itself.
//
// `ConnectionName` is the user-facing connection identifier —
// the `name` field on `(w17.module).connection`. Q1 of
// `docs/archive/iteration-2-multidb.md` restricts each domain to at
// most one connection per dialect, so a single name picks one
// dialect uniquely.
//
// `Timeout`, when > 0, wraps the tx's ctx with
// `context.WithTimeout`; on expiry the sql package auto-rolls
// back the tx (see [Memory.Begin]). Tier 2 of the §M2-F
// salamonsky two-tier model: `(w17.db.method).timeout_ms`
// (Tier 1) bounds individual statement work; this Timeout
// bounds the long-running tx-as-an-entity across multiple
// method calls.
type BeginOptions struct {
	ConnectionName string
	TxOptions      *sql.TxOptions
	Timeout        time.Duration
}

// NewMemory constructs an empty in-memory registry backed by a
// connection_name → *sql.DB map. Every entry's *sql.DB must be
// non-nil + outlive every tx the registry hands out.
//
// Single-connection domains pass a one-element map (e.g.
// `{"main": dbPostgres}`); multi-dialect domains pass one
// entry per declared connection (`{"main": dbPostgres, "audit":
// dbSqlite}`).
func NewMemory(dbs map[string]*sql.DB) *Memory {
	if len(dbs) == 0 {
		panic("txregistry.NewMemory: dbs map must not be empty")
	}
	for name, db := range dbs {
		if db == nil {
			panic(fmt.Sprintf("txregistry.NewMemory: db for connection %q is nil", name))
		}
	}
	// Defensive copy so caller mutations don't leak into the
	// registry's view.
	owned := make(map[string]*sql.DB, len(dbs))
	for k, v := range dbs {
		owned[k] = v
	}
	return &Memory{
		dbs: owned,
		txs: map[string]txEntry{},
	}
}

// Begin opens a fresh transaction on the *sql.DB matching
// opts.ConnectionName, assigns it a tx id, records the
// (txID → {tx, connName}) pair, and returns the assigned txID.
// Subsequent storage RPCs that carry `w17-tx-id = <txID>` adopt
// the tx via [LookupTx] — but only when the calling method's
// connection matches what's recorded here.
//
// Errors with a clear "unknown connection" message when
// opts.ConnectionName isn't in the registry's DB map (typical
// cause: caller passed a connection name the binary doesn't
// host, e.g. a typo). The gRPC server side maps this onto
// `codes.InvalidArgument` via [ErrUnknownConnection].
func (m *Memory) Begin(ctx context.Context, opts BeginOptions) (string, error) {
	db, ok := m.dbs[opts.ConnectionName]
	if !ok {
		return "", fmt.Errorf("%w: %q (registered: %v)", ErrUnknownConnection, opts.ConnectionName, m.knownConnections())
	}
	txCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		txCtx, cancel = context.WithTimeout(txCtx, opts.Timeout)
	}
	tx, err := db.BeginTx(txCtx, opts.TxOptions)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return "", fmt.Errorf("txregistry: BeginTx on %q: %w", opts.ConnectionName, err)
	}
	id := uuid.NewString()
	m.mu.Lock()
	m.txs[id] = txEntry{tx: tx, connName: opts.ConnectionName, cancel: cancel}
	m.mu.Unlock()
	if opts.Timeout > 0 {
		// Tier 2 background-watcher (slice 6Q-F follow-up):
		// when the tx ctx fires (deadline OR user-driven cancel
		// via Commit/Rollback), drain the registry entry. Race-
		// safe with user Commit/Rollback through the same mutex
		// + take(): whoever wins removes the entry; the other
		// gets ErrUnknownTxID and returns gracefully.
		//
		// Without this, deadline-fired txs leave their registry
		// entries dangling — Active() would over-report and the
		// next user Commit on that id would get an opaque
		// "Commit failed" instead of a clear ErrUnknownTxID.
		// Cost: one goroutine per Begin, parked on the ctx.Done
		// channel; releases on cancel. Cheap relative to the tx
		// itself.
		go m.watchTimeout(txCtx, id)
	}
	return id, nil
}

// watchTimeout drains the registry entry for `txID` when its
// ctx fires. Best-effort Rollback is issued on the underlying
// *sql.Tx — the sql package likely already auto-rolled back
// on deadline; the explicit Rollback handles the rare path
// where ctx was cancelled by something other than the
// deadline + the driver hadn't yet observed the cancellation.
//
// `ErrTxDone` is expected when the driver already closed the
// tx — silently ignored.
func (m *Memory) watchTimeout(ctx context.Context, txID string) {
	// Detached per-Begin goroutine: a panic here has no caller to
	// unwind into and would crash the storage process. There is no
	// reachable trigger today (the body only touches a guaranteed
	// non-nil *sql.Tx under the mutex) — this is defence in depth
	// against a future driver/wrapper whose Rollback could panic.
	defer func() {
		if r := recover(); r != nil {
			observx.ReportError(ctx, fmt.Errorf("PANIC txregistry watchTimeout %s: %v\n%s", txID, r, debug.Stack()))
		}
	}()
	<-ctx.Done()
	m.mu.Lock()
	entry, ok := m.txs[txID]
	if ok {
		delete(m.txs, txID)
	}
	m.mu.Unlock()
	if !ok {
		// User Commit / Rollback already drained the entry +
		// fired cancel(); nothing to clean up.
		return
	}
	_ = entry.tx.Rollback()
}

// Commit finalises the tx for txID and removes it from the
// registry. ErrUnknownTxID when the id doesn't resolve (the
// gRPC handler maps this onto a NotFound status). The
// underlying tx is rolled back if the Commit call returns
// an error so the connection isn't leaked into the pool with
// an open tx.
func (m *Memory) Commit(txID string) error {
	entry, err := m.take(txID)
	if err != nil {
		return err
	}
	// Release the WithTimeout goroutine (if Tier 2 timeout was
	// set on Begin) before doing the actual Commit — Commit
	// either succeeds (timer harmless) or fails (we want the
	// timer cleaned up either way).
	if entry.cancel != nil {
		defer entry.cancel()
	}
	if err := entry.tx.Commit(); err != nil {
		// Best-effort rollback — the tx is in a failed state and
		// must release its backend connection. Rollback after a
		// failed Commit is a no-op on most drivers but spelled
		// out for clarity. Deferred work is dropped: the writes it
		// would announce do not exist.
		_ = entry.tx.Rollback()
		return fmt.Errorf("txregistry: Commit %q: %w", txID, err)
	}
	// The writes are durable now — release the work that was waiting on
	// exactly this moment. Runs outside the registry lock so a callback
	// may re-enter the registry, and after the entry was taken, so a
	// late OnCommit for this id correctly reports "unknown".
	for _, fn := range entry.onCommit {
		fn()
	}
	return nil
}

// Rollback releases the tx for txID without committing.
// ErrUnknownTxID when the id doesn't resolve. Idempotent on
// the registry side: calling Rollback twice for the same id
// errors with ErrUnknownTxID (the second call has nothing to
// release).
func (m *Memory) Rollback(txID string) error {
	entry, err := m.take(txID)
	if err != nil {
		return err
	}
	if entry.cancel != nil {
		defer entry.cancel()
	}
	if err := entry.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("txregistry: Rollback %q: %w", txID, err)
	}
	return nil
}

// LookupTx implements [Registry]. Storage handlers call this
// via [AdoptTx] after reading the `w17-tx-id` metadata header.
//
// Three outcomes:
//   - tx_id unknown          → (nil, ErrUnknownTxID)
//   - tx_id known, conn match → (tx, nil)
//   - tx_id known, conn mismatch → (nil, ErrConnectionMismatch)
//
// The connection check is the M2-D correctness gate — a tx
// opened on connection A cannot be reused by a method on
// connection B (different `*sql.DB`, different backend
// transaction). AdoptTx propagates the mismatch error to the
// generator, which renders it as `codes.InvalidArgument`.
func (m *Memory) LookupTx(txID, connectionName string) (*sql.Tx, error) {
	m.mu.Lock()
	entry, ok := m.txs[txID]
	m.mu.Unlock()
	if !ok {
		return nil, ErrUnknownTxID
	}
	if entry.connName != connectionName {
		return nil, fmt.Errorf("%w: tx_id opened on %q, method on %q", ErrConnectionMismatch, entry.connName, connectionName)
	}
	return entry.tx, nil
}

// Active reports the number of in-flight transactions. Useful
// for liveness probes / metrics ("are we leaking txs?").
func (m *Memory) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.txs)
}

// take pops the entry for txID and returns it. ErrUnknownTxID
// when the id doesn't resolve.
// OnCommit registers fn to run after the tx for txID commits
// successfully. Reports whether the id resolved: false means there is no
// such tx in flight, and the caller must decide what to do instead
// (the emit wrappers run the work immediately — nothing is pending, so
// there is nothing to wait for).
//
// Callbacks run in registration order, outside the registry lock, after
// the underlying Commit returns nil. A failed Commit and any Rollback
// drop them: the writes are gone, so the events must not be published.
func (m *Memory) OnCommit(txID string, fn func()) bool {
	if fn == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.txs[txID]
	if !ok {
		return false
	}
	entry.onCommit = append(entry.onCommit, fn)
	m.txs[txID] = entry
	return true
}

func (m *Memory) take(txID string) (txEntry, error) {
	m.mu.Lock()
	entry, ok := m.txs[txID]
	if ok {
		delete(m.txs, txID)
	}
	m.mu.Unlock()
	if !ok {
		return txEntry{}, ErrUnknownTxID
	}
	return entry, nil
}

// knownConnections returns the registered connection names in
// no particular order. Surfaced in the ErrUnknownConnection
// diagnostic so the operator can spot typos.
func (m *Memory) knownConnections() []string {
	out := make([]string, 0, len(m.dbs))
	for k := range m.dbs {
		out = append(out, k)
	}
	return out
}

// ErrUnknownConnection is returned when Begin receives a
// connection name the registry doesn't host. The gRPC server
// side maps this onto `codes.InvalidArgument` (caller's name
// doesn't match anything the binary serves).
var ErrUnknownConnection = errors.New("txregistry: unknown connection")
