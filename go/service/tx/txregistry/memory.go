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
// `tx_id → *sql.Tx` map for one storage binary. This is the
// default deployment shape for small-to-mid-sized projects:
// one storage binary per domain → no cross-binary `conn_id`
// routing axis required.
//
// Memory holds one
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
// Memory holds its transactions in process memory, so one
// instance per domain is the deployment shape. Several replicas
// need a routing proxy in front that pins every call of a
// transaction to the replica that opened it (`w17-conn-id`).
// Without one, a call that lands on another replica carries an id
// that replica does not hold and fails with [ErrUnknownTxID] — it
// never runs in a fresh transaction of its own (see [AdoptTx]).
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

	// outcomes remembers, for the last [tombstoneCap] drained ids, WHY
	// the entry is gone. An id that resolved once and no longer does is
	// the single most confusing state this registry produces — the
	// caller is holding an id its own Begin returned, and every surface
	// answers "unknown", which reads as expired plumbing rather than as
	// the consequence of something the caller itself did three RPCs
	// earlier. Keeping the reason turns that into a sentence naming the
	// culprit (see [Memory.explainUnknownLocked]).
	//
	// Diagnostic only — nothing branches on it. `order` is the
	// insertion sequence backing the eviction of the oldest entry, so
	// the map cannot grow with the process.
	outcomes     map[string]string
	outcomeOrder []string
}

// tombstoneCap bounds how many drained tx ids keep their reason. The
// window only has to cover the gap between a transaction being drained
// and its coordinator noticing — a Commit that follows within the same
// flow — so a few hundred is generously past the point of usefulness
// while staying negligible (an id plus a short sentence each).
const tombstoneCap = 256

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
	// declaration instead of silently running weaker.
	// `sql.LevelDefault` means the caller pinned nothing, so
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

	// doomed is set the moment a rollback is asked for — before that
	// rollback waits for the lease — and is never cleared. A doomed
	// transaction can no longer commit or be adopted; it is rolled
	// back by whichever gets to it first: the rollback that doomed it,
	// a Commit that finds it idle, the release of the last lease (see
	// [Memory.release]) or the timeout watcher.
	//
	// The flag is what keeps "a failed participant discards the
	// transaction" true when the rollback itself cannot run yet. Before
	// it existed, a rollback that gave up waiting (a sibling held the
	// lease, its context was done) left the entry live and unmarked,
	// and the coordinator's later Commit succeeded over a failed call.
	//
	// doomCause is the outcome recorded once the entry is drained.
	doomed    bool
	doomCause string
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
// the `name` field on `(w17.module).connection`. Each domain has at
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
		outcomes:   map[string]string{},
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

// noteOutcomeLocked records why txID is no longer in the registry,
// evicting the oldest reason once [tombstoneCap] is reached. Caller
// must hold m.mu.
func (m *Memory) noteOutcomeLocked(txID, reason string) {
	if _, seen := m.outcomes[txID]; !seen {
		if len(m.outcomeOrder) >= tombstoneCap {
			delete(m.outcomes, m.outcomeOrder[0])
			m.outcomeOrder = m.outcomeOrder[1:]
		}
		m.outcomeOrder = append(m.outcomeOrder, txID)
	}
	m.outcomes[txID] = reason
}

// explainUnknownLocked builds the [ErrUnknownTxID] every surface
// returns for an id this registry does not hold, appending the
// recorded reason when there is one. Caller must hold m.mu.
//
// The error still wraps the sentinel — callers branch on
// `errors.Is(err, ErrUnknownTxID)` and the gRPC layer still maps it to
// NotFound. Only the human-readable half changes, which is the half
// that was pointing at the wrong thing.
func (m *Memory) explainUnknownLocked(txID string) error {
	if reason, ok := m.outcomes[txID]; ok {
		return fmt.Errorf("%w %q: %s", ErrUnknownTxID, txID, reason)
	}
	return fmt.Errorf("%w %q", ErrUnknownTxID, txID)
}

// noteOutcome is [Memory.noteOutcomeLocked] for callers that do not
// already hold the mutex.
func (m *Memory) noteOutcome(txID, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noteOutcomeLocked(txID, reason)
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
		m.noteOutcomeLocked(txID, "the transaction exceeded its tx_timeout_ms and was rolled back by the registry's orphan watcher")
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
	entry, err := m.take(ctx, txID, "the transaction was committed", true)
	if err != nil {
		return err
	}
	if entry.doomed {
		// A rollback was asked for while a handler held the lease; the
		// entry reached us idle before the deferred rollback ran. Run it
		// now instead of committing.
		m.rollbackDrained(ctx, txID, entry)
		return doomedError(txID, entry.doomCause)
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
		m.noteOutcome(txID, "the underlying database transaction failed to commit")
		return fmt.Errorf("txregistry: Commit %q: %w", txID, err)
	}
	// The writes are durable now — release the work that was waiting on
	// exactly this moment. Runs outside the registry lock so a callback
	// may re-enter the registry, and after the entry was taken, so a
	// late OnCommit for this id correctly reports "unknown".
	//
	// Each callback runs under its own recover. The transaction IS
	// committed whatever a callback does: a panic escaping here would
	// skip every later callback (other methods' events) and reach the
	// coordinator as a failed Commit for a write that landed. The
	// in-process path (txscope) recovers per hook for the same reason.
	for i, fn := range entry.onCommit {
		if p := runHook(fn); p != nil {
			observx.ReportError(context.WithoutCancel(ctx), fmt.Errorf(
				"txregistry: after-commit work #%d of committed transaction %s panicked: %v\n%s",
				i+1, txID, p.value, p.stack))
		}
	}
	return nil
}

// hookPanic is what [runHook] recovered.
type hookPanic struct {
	value any
	stack []byte
}

// runHook runs one after-commit callback and returns what it panicked
// with, or nil.
func runHook(fn func()) (p *hookPanic) {
	defer func() {
		if r := recover(); r != nil {
			p = &hookPanic{value: r, stack: debug.Stack()}
		}
	}()
	fn()
	return nil
}

// doomedError is what a Commit or an adoption gets for a transaction a
// rollback was already asked for.
func doomedError(txID, cause string) error {
	return fmt.Errorf("%w %q: %s", ErrTxDoomed, txID, cause)
}

// rollbackDrained rolls back an entry that was already removed from
// the registry for being doomed. A failure is reported rather than
// returned: the caller that drained it did not ask for this rollback,
// and the only thing it could do — release the connection — the driver
// has already done or will do on its own.
func (m *Memory) rollbackDrained(ctx context.Context, txID string, entry txEntry) {
	if entry.cancel != nil {
		defer entry.cancel()
	}
	if err := entry.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		observx.ReportError(context.WithoutCancel(ctx), fmt.Errorf(
			"txregistry: rollback of doomed transaction %s: %w", txID, err))
	}
}

// doomLocked marks txID as doomed with `outcome` as the reason it will be
// gone. The first reason wins: it names what actually discarded the
// transaction. Caller must hold m.mu.
func (m *Memory) doomLocked(txID, outcome string) {
	entry, ok := m.txs[txID]
	if !ok || entry.doomed {
		return
	}
	entry.doomed = true
	entry.doomCause = outcome
	m.txs[txID] = entry
}

// drainIfDoomedLocked removes a doomed entry that nobody is using or
// waiting on any more, returning it for [Memory.rollbackDrained]. Caller
// must hold m.mu.
func (m *Memory) drainIfDoomedLocked(txID string, entry txEntry) (txEntry, bool) {
	if !entry.doomed || entry.adopters > 0 || entry.finishers > 0 {
		return txEntry{}, false
	}
	delete(m.txs, txID)
	m.noteOutcomeLocked(txID, entry.doomCause)
	return entry, true
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
	return m.RollbackCaused(ctx, txID, "")
}

// RollbackCaused is [Memory.Rollback] with a note of what asked for
// it — the optional cause-carrying form the auto-rollback interceptor
// (`grpcrollback.CausedRoller`) prefers when the registry offers it.
//
// The rollback itself is identical — `cause` is remembered only so
// that the NEXT surface to be asked about this id can say why it is
// gone. That matters most for the caller that did not ask for the
// rollback: when the auto-rollback interceptor discards a transaction
// because one call inside it failed, the coordinator's own Commit is
// the first place the consequence shows up, and without the cause it
// reads as broken transaction plumbing rather than as its own tolerated
// error three calls back.
//
// An empty `cause` means the holder asked for this directly and needs
// no explanation.
//
// The transaction is DOOMED before the wait for an outstanding lease
// begins: from then on it cannot commit or be adopted, whether or not
// the wait succeeds. When the wait gives up (ctx done, finish-wait
// expired) the error wraps [ErrTxDoomed] as well as [ErrTxBusy], and the
// rollback runs as soon as the last lease is released.
func (m *Memory) RollbackCaused(ctx context.Context, txID, cause string) error {
	outcome := "the transaction was rolled back"
	if cause != "" {
		outcome = "the transaction was rolled back: " + cause
	}
	// Doomed BEFORE waiting for the lease. Whatever happens to the wait
	// — the caller's context ends, the finish-wait runs out — the
	// transaction can no longer commit: asking for a rollback is a
	// decision, and a decision that could be lost to a timing detail
	// would let a transaction with a failed participant commit.
	m.mu.Lock()
	m.doomLocked(txID, outcome)
	m.mu.Unlock()
	entry, err := m.take(ctx, txID, outcome, false)
	if err != nil {
		if errors.Is(err, ErrTxBusy) {
			// Not "left exactly as it was" (the ErrTxBusy contract for a
			// Commit): the rollback is merely deferred to the moment the
			// running handler releases its lease.
			return fmt.Errorf("%w %q: rollback deferred until the running handler releases it: %w", ErrTxDoomed, txID, err)
		}
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
// The lease is the one-adopter-at-a-time gate: the returned tx runs
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
			err := m.explainUnknownLocked(txID)
			m.mu.Unlock()
			return nil, noopRelease, err
		}
		if entry.connName != connectionName {
			m.mu.Unlock()
			return nil, noopRelease, fmt.Errorf("%w: tx_id opened on %q, method on %q", ErrConnectionMismatch, entry.connName, connectionName)
		}
		if entry.doomed {
			// A rollback was asked for and is waiting for the current
			// holder. New work must not join a transaction that can only
			// be rolled back.
			m.mu.Unlock()
			return nil, noopRelease, doomedError(txID, entry.doomCause)
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
	// The rollback that doomed this transaction gave up waiting for this
	// lease; nobody else is queued to finish it, so it ends here.
	drained, ok := m.drainIfDoomedLocked(txID, entry)
	m.mu.Unlock()
	if idle != nil {
		close(idle)
	}
	if ok {
		m.rollbackDrained(context.Background(), txID, drained)
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
// The wait is the no-split-method gate. `database/sql`'s
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
//
// `commit` marks the caller as a Commit: a doomed entry is refused
// without waiting (it can never commit), and one found idle is returned
// with `doomed` set for the caller to roll back instead.
//
// `outcome` is recorded against the id in the same critical section
// that removes it, so a racing Commit / LookupTx that misses the entry
// by a hair still gets told what happened to it rather than a bare
// "unknown". The finisher refines it afterwards if the underlying
// Commit / Rollback then fails.
func (m *Memory) take(ctx context.Context, txID, outcome string, commit bool) (txEntry, error) {
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
			err := m.explainUnknownLocked(txID)
			m.mu.Unlock()
			return txEntry{}, err
		}
		if entry.adopters == 0 {
			delete(m.txs, txID)
			if entry.doomed {
				// Whoever finishes a doomed entry rolls it back, so the
				// recorded outcome is the doom's, not the caller's.
				m.noteOutcomeLocked(txID, entry.doomCause)
			} else {
				m.noteOutcomeLocked(txID, outcome)
			}
			m.mu.Unlock()
			announced = false
			return entry, nil
		}
		if commit && entry.doomed {
			// No point waiting for the holder: the answer is already
			// known, and the rollback runs when the lease is released.
			m.mu.Unlock()
			return txEntry{}, doomedError(txID, entry.doomCause)
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
	entry, ok := m.txs[txID]
	if !ok || entry.finishers == 0 {
		m.mu.Unlock()
		return
	}
	entry.finishers--
	m.txs[txID] = entry
	// A doomed entry whose last waiting finisher just gave up, after the
	// lease was released in the meantime: nobody else will finish it.
	drained, ok := m.drainIfDoomedLocked(txID, entry)
	m.mu.Unlock()
	if ok {
		m.rollbackDrained(context.Background(), txID, drained)
	}
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
