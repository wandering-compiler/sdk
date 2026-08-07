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

	// finishWait caps how long a finisher waits for an adopted
	// handler to release, and how long a second adopter waits
	// for the first. See [DefaultFinishWait] / [WithFinishWait].
	finishWait time.Duration
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

	// iso is the isolation level this tx was OPENED with — the
	// only moment it can be decided, since no engine lets a running
	// transaction change level. [Memory.IsolationFor] reports it so
	// a method that DECLARES `(w17.db.method).tx_isolation` can
	// refuse to adopt a transaction that cannot satisfy the
	// declaration instead of silently running weaker (T3-7 pass #9
	// D-F7). `sql.LevelDefault` means the caller pinned nothing, so
	// the level is whatever the driver chose — unknown here, and
	// therefore never strong enough for a declared level.
	iso sql.IsolationLevel

	// onCommit holds work deferred until this tx actually commits —
	// today, the eventbus emits of every method that ran inside it.
	// A method that ADOPTS a caller's tx does not commit; the caller
	// does, later. Emitting when such a method returns publishes an
	// event for a write that is still provisional, so a rollback
	// leaves subscribers acting on a mutation that never happened.
	// Callbacks run after a successful Commit, and are discarded on
	// Rollback.
	onCommit []func()

	// adopters counts the handlers currently holding an adoption
	// lease on this tx — i.e. the ones that are between two of
	// their own statements. It is 0 or 1 today (LookupTx is
	// exclusive) but is a count, not a flag, so the exclusivity
	// rule lives in one place and a future shared-read lease has
	// somewhere to go.
	//
	// `idle` is non-nil exactly while adopters > 0 and is closed
	// when the count drops back to 0 (or when the entry is
	// force-drained), waking everyone parked on the lease.
	// `finishers` counts the Commit / Rollback calls waiting their
	// turn, so no new adopter can queue ahead of them and starve
	// one.
	adopters  int
	idle      chan struct{}
	finishers int
}

// DefaultFinishWait bounds two symmetric waits: a Commit /
// Rollback waiting for an adopted handler to finish its
// statements, and a second adopter waiting for the first to
// release. On expiry the waiter gets [ErrTxBusy] and the
// transaction is left untouched.
//
// It exists so a leaked lease (a handler goroutine wedged on
// something else) degrades to a clear, retryable error instead
// of wedging the coordinator with it. The value is an upper
// bound on "a handler that is still making progress", not a
// tuning knob: per-statement work is already bounded far below
// it by the Tier-1 `(w17.db.method).timeout_ms`, while the
// Tier-2 orphan default ([distx.DefaultOrphanTimeout], 5 min)
// stays an order of magnitude above so the two guards never
// race to explain the same stall. Operators tune via
// [WithFinishWait].
const DefaultFinishWait = 30 * time.Second

// MemoryOption configures a [Memory] at construction time.
type MemoryOption func(*Memory)

// WithFinishWait overrides [DefaultFinishWait]. A non-positive
// value means "do not wait at all": a finisher or a second
// adopter that meets an outstanding lease fails immediately
// with [ErrTxBusy].
func WithFinishWait(d time.Duration) MemoryOption {
	return func(m *Memory) { m.finishWait = d }
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
func NewMemory(dbs map[string]*sql.DB, opts ...MemoryOption) *Memory {
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
	m := &Memory{
		dbs:        owned,
		txs:        map[string]txEntry{},
		finishWait: DefaultFinishWait,
	}
	for _, o := range opts {
		o(m)
	}
	return m
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
	iso := sql.LevelDefault
	if opts.TxOptions != nil {
		iso = opts.TxOptions.Isolation
	}
	id := uuid.NewString()
	m.mu.Lock()
	m.txs[id] = txEntry{tx: tx, connName: opts.ConnectionName, cancel: cancel, iso: iso}
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
	// Deliberately NOT lease-aware, unlike Commit / Rollback.
	// This is the reclaimer of last resort and its ctx has
	// already fired: the sql package auto-rolled the tx back on
	// the deadline, so an adopted handler's next statement fails
	// whether or not we wait, and waiting would pin the entry
	// (plus its pooled connection) for as long as that handler
	// stays wedged — the exact leak the watcher exists to bound.
	// Waking the waiters keeps the invariant that `idle` is
	// always closed exactly once: a lease released after this
	// finds no entry and does nothing.
	if ok && entry.idle != nil {
		close(entry.idle)
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
//
// ctx bounds the wait for an adopted handler that is still
// issuing statements (see [Memory.take]); [ErrTxBusy] when that
// wait runs out, with the transaction left open and retryable.
func (m *Memory) Commit(ctx context.Context, txID string) error {
	entry, err := m.take(ctx, txID)
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
//
// ctx bounds the wait for an outstanding adoption lease exactly
// as [Memory.Commit] does — a rollback that lands between two
// statements of a running handler is self-consistent on the
// database side, but it makes that handler fail halfway with a
// bare `sql.ErrTxDone`.
func (m *Memory) Rollback(ctx context.Context, txID string) error {
	entry, err := m.take(ctx, txID)
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
// Four outcomes:
//   - tx_id unknown              → (nil, noop, ErrUnknownTxID)
//   - tx_id known, conn mismatch → (nil, noop, ErrConnectionMismatch)
//   - tx_id known, held by someone else past the wait, or being
//     finished → (nil, noop, ErrTxBusy)
//   - tx_id known, conn match    → (tx, release, nil)
//
// The connection check is the M2-D correctness gate — a tx
// opened on connection A cannot be reused by a method on
// connection B (different `*sql.DB`, different backend
// transaction). AdoptTx propagates the mismatch error to the
// generator, which renders it as `codes.InvalidArgument`.
//
// The lease is the T3-7 pass #7 C-F3 gate: the returned tx runs
// on ONE backend connection, so two handlers issuing sequences
// of statements on it interleave inside a single PostgreSQL
// transaction, and a locked query holding an open `*sql.Rows`
// concurrently with a sibling's mutation desyncs the driver
// protocol. A second caller therefore WAITS for the first to
// release — serialising them rather than refusing, so a caller
// that fans out over one tx keeps working — bounded by ctx and
// by the registry's finish-wait cap.
func (m *Memory) LookupTx(ctx context.Context, txID, connectionName string) (*sql.Tx, ReleaseFunc, error) {
	// The timer is created on the first wait, so the uncontended
	// adoption — every adoption in a sequential flow — costs one
	// mutex round-trip and no allocation.
	var timeout <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		m.mu.Lock()
		entry, ok := m.txs[txID]
		if !ok {
			m.mu.Unlock()
			return nil, noopRelease, ErrUnknownTxID
		}
		if entry.connName != connectionName {
			m.mu.Unlock()
			return nil, noopRelease, fmt.Errorf("%w: tx_id opened on %q, method on %q", ErrConnectionMismatch, entry.connName, connectionName)
		}
		if entry.finishers > 0 {
			// A Commit / Rollback is waiting its turn. Joining the
			// queue behind it would only earn an ErrUnknownTxID once
			// it wins, so say so now.
			m.mu.Unlock()
			return nil, noopRelease, fmt.Errorf("%w: tx_id %q is being committed or rolled back", ErrTxBusy, txID)
		}
		if entry.adopters == 0 {
			entry.adopters = 1
			entry.idle = make(chan struct{})
			m.txs[txID] = entry
			tx := entry.tx
			m.mu.Unlock()
			return tx, m.releaser(txID), nil
		}
		idle := entry.idle
		m.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(m.finishWait)
			timeout = timer.C
		}
		select {
		case <-idle:
			// The holder released (or the entry was drained); re-read
			// the entry and try again.
		case <-ctx.Done():
			return nil, noopRelease, fmt.Errorf("%w: tx_id %q held by another caller: %w", ErrTxBusy, txID, ctx.Err())
		case <-timeout:
			return nil, noopRelease, fmt.Errorf("%w: tx_id %q still held after %s", ErrTxBusy, txID, m.finishWait)
		}
	}
}

// releaser builds the [ReleaseFunc] handed to one adopter. It
// is idempotent: a second call after the count already dropped
// (or after the entry was force-drained) does nothing.
func (m *Memory) releaser(txID string) ReleaseFunc {
	var once sync.Once
	return func() {
		once.Do(func() { m.release(txID) })
	}
}

func (m *Memory) release(txID string) {
	m.mu.Lock()
	entry, ok := m.txs[txID]
	if !ok || entry.adopters == 0 {
		// Force-drained by watchTimeout while we held it; that path
		// already closed `idle` and woke the waiters.
		m.mu.Unlock()
		return
	}
	entry.adopters--
	var idle chan struct{}
	if entry.adopters == 0 {
		idle, entry.idle = entry.idle, nil
	}
	m.txs[txID] = entry
	m.mu.Unlock()
	if idle != nil {
		close(idle)
	}
}

// IsolationFor reports the isolation level txID was opened with,
// implementing [IsolationReporter]. `sql.LevelDefault` is returned both
// for an id this registry does not hold and for a transaction whose
// caller pinned nothing — the two are the same answer to the only
// question the caller is asking ("can this transaction satisfy a
// declared level?"), and both mean no.
func (m *Memory) IsolationFor(txID string) sql.IsolationLevel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.txs[txID].iso
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

// take pops the entry for txID and returns it, waiting first
// for any outstanding adoption lease to be released.
//
// The wait is the T3-7 pass #7 C-F2 gate. `database/sql`'s
// close-mutex only holds off the statement IN FLIGHT, not a
// handler that intends more statements, so a finisher that
// takes the entry between statement N and N+1 of an adopted
// method makes N durable and N+1 fail with `sql.ErrTxDone`: the
// method reports failure to its caller while half of it is
// committed, and nothing anywhere says so. Waiting for the
// lease preserves per-method atomicity; when the wait runs out
// the finisher fails with [ErrTxBusy] and leaves the tx open
// rather than splitting a method in half.
//
// `finishing` is published before parking so no new adopter can
// queue ahead and starve the finisher.
func (m *Memory) take(ctx context.Context, txID string) (txEntry, error) {
	var timeout <-chan time.Time
	var timer *time.Timer
	announced := false
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		if announced {
			// Give-up path: withdraw the intent so the transaction
			// stays adoptable and a retry can succeed. (Reached on
			// every non-nil-error return; the success path deletes
			// the entry, and withdrawing from a deleted entry is a
			// no-op.)
			m.withdrawFinisher(txID)
		}
	}()
	for {
		m.mu.Lock()
		entry, ok := m.txs[txID]
		if !ok {
			m.mu.Unlock()
			return txEntry{}, ErrUnknownTxID
		}
		if entry.adopters == 0 {
			delete(m.txs, txID)
			m.mu.Unlock()
			announced = false
			return entry, nil
		}
		if !announced {
			entry.finishers++
			announced = true
		}
		idle := entry.idle
		m.txs[txID] = entry
		m.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(m.finishWait)
			timeout = timer.C
		}
		select {
		case <-idle:
			// The adopter released; loop and take the entry.
		case <-ctx.Done():
			return txEntry{}, fmt.Errorf("%w: tx_id %q still has a handler running: %w", ErrTxBusy, txID, ctx.Err())
		case <-timeout:
			return txEntry{}, fmt.Errorf("%w: tx_id %q still has a handler running after %s", ErrTxBusy, txID, m.finishWait)
		}
	}
}

// withdrawFinisher drops one waiting-finisher intent. A count, not
// a flag: two finishers racing the same id must not have one's
// give-up clear the other's intent and let a fresh adopter starve
// it.
func (m *Memory) withdrawFinisher(txID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.txs[txID]
	if !ok || entry.finishers == 0 {
		return
	}
	entry.finishers--
	m.txs[txID] = entry
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
