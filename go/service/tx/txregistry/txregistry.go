// Package txregistry provides the interface generated storage
// handlers use to adopt a caller-supplied transaction. It is the
// runtime hook for the spec's distributed-tx model
// (`docs/archive/iteration-2-dql.md` §"Single-connection mutations" +
// `docs/archive/iteration-2-multidb.md` §M2-D for connection routing):
//
//	caller → W17DistributedTransaction.Begin({connection_name}) → returns (conn_id, tx_id)
//	caller → ServiceA.MethodFoo(req, metadata={w17-tx-id})
//	         ↓ generated handler reads metadata, looks tx_id up
//	           on its connection, runs inside the open tx
//	caller → W17DistributedTransaction.Commit(tx_id)
//
// If the metadata is absent (or the registry is nil), the
// generated handler opens its own tx scoped to the method call —
// the slice 6H "always-fresh-tx" fallback. If a tx_id IS present
// but unusable — unknown to this registry ([ErrUnknownTxID]), or
// opened on a DIFFERENT connection than the calling method
// ([ErrConnectionMismatch]) — AdoptTx errors, and the generator
// routes that to a gRPC `INVALID_ARGUMENT` so the caller sees a
// clear diagnostic instead of silently committing partial state
// on a fresh tx.
//
// The Registry implementation lives per-domain. M2-B shipped
// the single-connection default; M2-D extends Memory to dispatch
// Begin / LookupTx by connection_name so multi-dialect domains
// route correctly.
package txregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// HeaderName is the gRPC metadata key the spec assigns to the
// adopted-tx identifier (lowercase per gRPC's canonical-form
// convention for ASCII headers).
const HeaderName = "w17-tx-id"

// DBOrTx is the subset of `database/sql` methods both `*sql.DB`
// and `*sql.Tx` satisfy — the shape generated query-with-lock
// handlers (REV-046) use to switch between the pool and an
// adopted tx without re-templating the per-row scan body. When
// the handler adopts a caller-supplied tx (`w17-tx-id` metadata
// resolves on this connection), `conn` is the `*sql.Tx`; when no
// tx is adopted, `conn` is `s.dbPostgres` (or the matching
// dialect-named field) — the body's `conn.QueryRowContext` /
// `conn.QueryContext` calls dispatch through the interface in
// either case.
//
// Single hop into the database/sql call sites — the interface
// method set matches exactly the methods generated body
// templates emit (QueryRowContext, QueryContext, ExecContext).
// ExecContext is unused by query-only methods today; it lands
// to keep the type usable from intermediate-op bodies that
// share the conn (out of scope for REV-046 v1 but cheap to
// reserve).
type DBOrTx interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ReleaseFunc ends an adoption lease. Every successful
// [Registry.LookupTx] returns one and the caller MUST run it
// (`defer`) when it is done issuing statements on the tx —
// that is what tells the registry the handler is no longer
// between two statements. Calling it more than once, or on the
// no-op returned alongside an error, is safe.
type ReleaseFunc func()

// Registry resolves a (w17-tx-id, connection_name) pair to the
// live *sql.Tx the W17DistributedTransaction service opened on
// that connection. Generated handlers hold a Registry on their
// server struct; nil is allowed and short-circuits AdoptTx to
// the fresh-tx path.
//
// The interface is intentionally minimal — single read
// operation, no Begin / Commit / Rollback (those live on the
// W17DistributedTransaction RPC surface, not on the storage
// handler's view of the registry). Implementations are
// expected to be safe for concurrent use.
//
// **Adoption is leased, and a lease is exclusive.** A tx runs
// on ONE backend connection: `database/sql` serialises the
// submission of individual statements on it, but nothing
// serialises two handlers that each intend a SEQUENCE of them,
// and an open `*sql.Rows` from a locked query concurrent with a
// sibling's mutation desyncs the driver protocol outright
// (lib/pq multiplexes nothing). So LookupTx hands a tx to one
// adopter at a time: a second caller waits for the first to
// release, bounded by ctx (and by the implementation's own
// cap), then gets [ErrTxBusy]. Symmetrically, a finisher
// (Commit / Rollback) may not close a tx while a lease is
// outstanding — otherwise it lands between two statements of a
// running handler, making statement 1 durable and statement 2
// fail with `sql.ErrTxDone` while the method reports failure
// (T3-7 pass #7 C-F2/C-F3).
//
// The connection_name parameter is the calling method's
// connection (from `(w17.module).connection.name`).
// Implementations MUST refuse to return a tx that was opened on
// a different connection — cross-connection tx adoption is a
// correctness violation. Mismatch is signalled via
// [ErrConnectionMismatch]; unknown tx_id via [ErrUnknownTxID];
// a lease that could not be acquired in time via [ErrTxBusy].
type Registry interface {
	LookupTx(ctx context.Context, txID, connectionName string) (*sql.Tx, ReleaseFunc, error)
}

// noopRelease is the [ReleaseFunc] returned on every path that
// acquired no lease, so a call site can `defer release()`
// unconditionally.
func noopRelease() {}

// ErrUnknownTxID is returned when LookupTx (or Commit /
// Rollback on Memory) receives an id the registry doesn't
// hold. Typical causes: caller already closed the tx, the id
// was never opened by Begin, the id was minted by ANOTHER
// bundle's registry, the binary restarted between Begin and
// Commit (in-memory state dropped). AdoptTx SURFACES it — a
// caller that asserted a tx id and cannot be joined gets an
// error, never a different, independently-committing
// transaction.
var ErrUnknownTxID = errors.New("txregistry: unknown tx_id")

// ErrConnectionMismatch is returned when the calling method's
// connection_name doesn't match the connection the tx was
// opened on. Generator wraps this in a `codes.InvalidArgument`
// status so the caller sees a clear diagnostic; the alternative
// (silent fall-through to a fresh tx) would split the caller's
// logical transaction across two connections without warning.
var ErrConnectionMismatch = errors.New("txregistry: tx_id was opened on a different connection")

// ErrTxBusy is returned when a lease could not be acquired
// because another handler still holds the transaction, or when
// a Commit / Rollback could not run because an adopted handler
// is still issuing statements on it. Both directions are
// bounded waits, not refusals-on-sight: the error means the
// wait ran out, and the transaction is left exactly as it was —
// still open, still adoptable, safe to retry.
var ErrTxBusy = errors.New("txregistry: tx_id is in use by another caller")

// AdoptTx reads the w17-tx-id metadata header off ctx and
// resolves it through reg against the calling method's
// connection_name. Three outcomes:
//
//   - (tx, true, release, nil)   → caller's tx_id matched on
//     this connection; the generator uses the returned tx, the
//     caller is responsible for Commit / Rollback.
//   - (nil, false, release, nil) → no tx_id present (nil
//     registry, no metadata, empty header). The caller claimed
//     no transaction, so the generator opens a fresh per-method
//     tx.
//   - (nil, false, release, err) → tx_id present but unusable:
//     this registry does not hold it (ErrUnknownTxID — stale, or
//     minted by another bundle's registry), the tx was opened
//     on a different connection (ErrConnectionMismatch), or
//     another handler holds it and did not release in time
//     (ErrTxBusy). The generator surfaces this to the client as
//     `codes.InvalidArgument`. Falling through to a fresh tx
//     here would split the caller's logical transaction with no
//     error at all — see D-F4.
//
// The third return is always non-nil, so the generated preamble
// can `defer release()` right after the error check without
// branching. It ends the adoption lease: until it runs, no
// other handler adopts this tx and no Commit / Rollback closes
// it out from under this one (see [Registry]).
//
// connectionName is the file-level connection identifier from
// `(w17.module).connection.name` — generator threads it as
// a string literal into the call site of every generated
// mutation handler.
func AdoptTx(ctx context.Context, reg Registry, connectionName string) (*sql.Tx, bool, ReleaseFunc, error) {
	if reg == nil {
		return nil, false, noopRelease, nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, false, noopRelease, nil
	}
	vals := md.Get(HeaderName)
	if len(vals) == 0 || vals[0] == "" {
		return nil, false, noopRelease, nil
	}
	tx, release, err := reg.LookupTx(ctx, vals[0], connectionName)
	if release == nil {
		release = noopRelease
	}
	if err != nil {
		// Every registry error is surfaced, ErrUnknownTxID
		// included. An id this registry does not hold is not the
		// same situation as no id at all: the caller asserted it
		// is inside a transaction, and this process cannot join
		// it. Opening a fresh one instead makes the method commit
		// independently of the transaction it was told to be part
		// of — the caller's Rollback then rolls back an empty tx
		// and the write survives, with no error anywhere (T2-6
		// pass #8 D-F4, silent arm). The interesting cause is an
		// id minted by ANOTHER bundle's registry, which is
		// indistinguishable from a stale one here and needs the
		// same answer.
		release()
		return nil, false, noopRelease, err
	}
	return tx, true, release, nil
}

// IsolationReporter is the optional slice of a [Registry] that can say
// what isolation level a transaction was opened with. [Memory]
// implements it. It is optional rather than part of Registry so an
// existing third-party implementation keeps compiling — but a registry
// that does not implement it can never satisfy a declared level, which
// is the fail-closed answer: [AdoptTxIsolated] refuses instead of
// assuming.
//
// `sql.LevelDefault` is the "cannot tell" answer (the caller pinned
// nothing, or the id is unknown) and never satisfies a declaration.
type IsolationReporter interface {
	IsolationFor(txID string) sql.IsolationLevel
}

// ErrIsolationTooWeak is returned when a method that DECLARES
// `(w17.db.method).tx_isolation` is called inside a transaction that
// was opened at a weaker (or unknown) level.
//
// No engine lets a running transaction change its isolation level, so
// the method cannot fix this from where it stands. The two honest
// answers are "honour the declaration" and "refuse"; the third —
// running weaker than declared, silently — is exactly the defect this
// error exists to end (T3-7 pass #9 D-F7). The caller's remedy is to
// pin the level on `W17DistributedTransaction.Begin`
// (`BeginRequest.isolation`).
var ErrIsolationTooWeak = errors.New("txregistry: open transaction is weaker than the method's declared tx_isolation")

// isolationRank orders the levels this compiler emits by strength, so
// "at least as strong as declared" is a comparison rather than a table
// of pairs. `sql.LevelDefault` ranks 0 — it means the level was never
// pinned, so nothing about it is known and it satisfies no declaration.
//
// Deliberately NOT the raw `sql.IsolationLevel` integer: that ordering
// interleaves WriteCommitted / Snapshot between the standard levels and
// says nothing useful about the four w17 declares.
func isolationRank(l sql.IsolationLevel) int {
	switch l {
	case sql.LevelReadUncommitted:
		return 1
	case sql.LevelReadCommitted:
		return 2
	case sql.LevelRepeatableRead:
		return 3
	case sql.LevelSerializable, sql.LevelLinearizable:
		return 4
	}
	return 0
}

// AdoptTxIsolated is [AdoptTx] for a method that declares
// `(w17.db.method).tx_isolation`. It adopts on the same three outcomes,
// with one added refusal: an adopted transaction whose level is weaker
// than `want` yields [ErrIsolationTooWeak] and no tx.
//
// The fresh-tx path is unaffected — a method that opens its own
// transaction already passes the declared level to BeginTx, and this
// function returns didAdopt=false for it exactly as AdoptTx does, so
// the generated preamble's `else` branch still runs.
//
// `want == sql.LevelDefault` degenerates to AdoptTx: nothing was
// declared, so nothing is enforced.
func AdoptTxIsolated(ctx context.Context, reg Registry, connectionName string, want sql.IsolationLevel) (*sql.Tx, bool, ReleaseFunc, error) {
	tx, didAdopt, release, err := AdoptTx(ctx, reg, connectionName)
	if err != nil || !didAdopt || want == sql.LevelDefault {
		return tx, didAdopt, release, err
	}
	got := sql.LevelDefault
	if r, ok := reg.(IsolationReporter); ok {
		got = r.IsolationFor(txIDFrom(ctx))
	}
	if isolationRank(got) < isolationRank(want) {
		release()
		return nil, false, noopRelease, fmt.Errorf("%w: transaction is %v, method declares %v", ErrIsolationTooWeak, got, want)
	}
	return tx, didAdopt, release, nil
}

// txIDFrom re-reads the adopted transaction id off ctx. AdoptTx has
// already validated it by the time this runs.
func txIDFrom(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if vals := md.Get(HeaderName); len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// CommitHook is the slice of a registry the emit wrappers need: a way to
// park work until the caller's transaction actually commits. [Memory]
// implements it. Kept separate from [Registry] so a consumer that only
// resolves transactions is unaffected.
type CommitHook interface {
	// OnCommit registers fn to run after the tx for txID commits.
	// Reports whether the id resolved to a tx in flight.
	OnCommit(txID string, fn func()) bool
}

// DeferOutcome is what [DeferUntilCommit] did with the work it was handed.
// Three states, because "the registry is not holding a transaction for this
// request" has two causes that need OPPOSITE answers — see [EmitNow] vs
// [EmitDropped].
type DeferOutcome int

const (
	// EmitNow — the request rides no transaction of anyone else's, so
	// there is nothing to wait for and the caller must run the work
	// itself. The ordinary case: a method that opens its own transaction
	// has already committed it by the time it returns.
	EmitNow DeferOutcome = iota

	// EmitDeferred — the work is parked on the caller's transaction and
	// will run if and when that transaction commits. The caller must NOT
	// run it.
	EmitDeferred

	// EmitDropped — the request DID ride someone else's transaction, and
	// that transaction is no longer in the registry: it was committed or
	// rolled back concurrently, in the window between the handler's last
	// statement and this call. The work must be thrown away. Running it
	// (the pre-T3-7-pass-#7 behaviour, which could not tell this state
	// from EmitNow) announces a write that a concurrent rollback has
	// already discarded — the phantom event the whole defer machinery
	// exists to prevent.
	EmitDropped
)

func (o DeferOutcome) String() string {
	switch o {
	case EmitNow:
		return "emit-now"
	case EmitDeferred:
		return "deferred"
	case EmitDropped:
		return "dropped"
	default:
		return fmt.Sprintf("DeferOutcome(%d)", int(o))
	}
}

// DeferUntilCommit parks fn until the transaction this request rides
// commits, and reports what it did.
//
// [EmitNow] means the request carries no `w17-tx-id` at all (or there is no
// registry to park on): the caller claimed no transaction, its method
// committed its own, and the work runs immediately.
//
// [EmitDeferred] means the caller adopted someone else's transaction. Such a
// method does NOT commit; the orchestrator does, later, after the remaining
// methods in the same transaction have run. Work that announces the write —
// an eventbus emit — must wait for that, or a rollback leaves subscribers
// acting on a mutation that never happened. On rollback fn is dropped.
//
// [EmitDropped] means the request carried a tx id the registry no longer
// holds. That is NOT the same situation as carrying none: an id this
// registry never held would have failed [AdoptTx] and the handler would have
// errored before reaching here, so a wrapper that gets this far had a live
// entry when it adopted. The entry can therefore only have been drained
// concurrently — by the Tier-2 timeout watcher, by the coordinator's
// Rollback RPC, or by the auto-rollback a sibling RPC's failure fires on the
// shared tx. Either way this method's writes are gone (or, for the
// commit-in-progress arm, not yet durable and possibly about to fail), so
// the announcement is dropped rather than published.
//
// fn runs on the committing goroutine, so it must not block for long.
// Its context should be detached from the request (`context.WithoutCancel`)
// — the RPC that queued it has already returned, and its ctx may well be
// cancelled by the time the commit lands.
func DeferUntilCommit(ctx context.Context, reg CommitHook, fn func()) DeferOutcome {
	if reg == nil || fn == nil {
		return EmitNow
	}
	txID := RequestTxID(ctx)
	if txID == "" {
		return EmitNow
	}
	if reg.OnCommit(txID, fn) {
		return EmitDeferred
	}
	// Dropping work silently is its own failure mode, so say so once,
	// on the path that is rare by construction.
	observx.ReportError(ctx, fmt.Errorf(
		"txregistry: dropped the deferred announcement of tx %q: the transaction was committed or rolled back before the emit registered", txID))
	return EmitDropped
}

// RequestTxID returns the `w17-tx-id` the caller put on the request, or
// "" when there is none. Same header [AdoptTx] resolves, read here
// without touching the registry so a caller can tell "is this request
// part of someone else's transaction?" on its own.
func RequestTxID(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(HeaderName)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
