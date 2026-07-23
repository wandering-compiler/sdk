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
// If the metadata is absent (or the registry is nil, or the
// tx_id is unknown), the generated handler opens its own tx
// scoped to the method call — the slice 6H "always-fresh-tx"
// fallback. If the tx_id resolves to a tx opened on a DIFFERENT
// connection than the calling method, AdoptTx errors with
// [ErrConnectionMismatch] — the generator routes that to a gRPC
// `INVALID_ARGUMENT` so the caller sees a clear "cross-connection
// reuse" diagnostic instead of silently committing partial
// state on a fresh tx.
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

	"google.golang.org/grpc/metadata"
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
// expected to be safe for concurrent use; a single tx_id maps
// to a single *sql.Tx the implementation hands back on every
// call until Commit / Rollback closes the entry.
//
// The connection_name parameter is the calling method's
// connection (from `(w17.module).connection.name`).
// Implementations MUST refuse to return a tx that was opened on
// a different connection — cross-connection tx adoption is a
// correctness violation. Mismatch is signalled via
// [ErrConnectionMismatch]; unknown tx_id via [ErrUnknownTxID].
type Registry interface {
	LookupTx(txID, connectionName string) (*sql.Tx, error)
}

// ErrUnknownTxID is returned when LookupTx (or Commit /
// Rollback on Memory) receives an id the registry doesn't
// hold. Typical causes: caller already closed the tx, the id
// was never opened by Begin, the binary restarted between
// Begin and Commit (in-memory state dropped). AdoptTx treats
// this as the "fall through to fresh tx" signal — same as
// the no-metadata path.
var ErrUnknownTxID = errors.New("txregistry: unknown tx_id")

// ErrConnectionMismatch is returned when the calling method's
// connection_name doesn't match the connection the tx was
// opened on. Generator wraps this in a `codes.InvalidArgument`
// status so the caller sees a clear diagnostic; the alternative
// (silent fall-through to a fresh tx) would split the caller's
// logical transaction across two connections without warning.
var ErrConnectionMismatch = errors.New("txregistry: tx_id was opened on a different connection")

// AdoptTx reads the w17-tx-id metadata header off ctx and
// resolves it through reg against the calling method's
// connection_name. Three outcomes:
//
//   - (tx, true, nil)        → caller's tx_id matched on this
//     connection; the generator uses the returned tx, the
//     caller is responsible for Commit / Rollback.
//   - (nil, false, nil)      → no tx_id present (nil registry,
//     no metadata, empty header) OR tx_id unknown to the
//     registry. The generator opens a fresh per-method tx.
//   - (nil, false, err)      → tx_id present but the tx was
//     opened on a different connection (ErrConnectionMismatch).
//     The generator surfaces this to the client as
//     `codes.InvalidArgument`.
//
// connectionName is the file-level connection identifier from
// `(w17.module).connection.name` — generator threads it as
// a string literal into the call site of every generated
// mutation handler.
func AdoptTx(ctx context.Context, reg Registry, connectionName string) (*sql.Tx, bool, error) {
	if reg == nil {
		return nil, false, nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, false, nil
	}
	vals := md.Get(HeaderName)
	if len(vals) == 0 || vals[0] == "" {
		return nil, false, nil
	}
	tx, err := reg.LookupTx(vals[0], connectionName)
	if err != nil {
		if errors.Is(err, ErrUnknownTxID) {
			// Stale tx_id (binary restarted, caller forgot to
			// drop the metadata after Commit, …). Fall through
			// to fresh tx — same behaviour as no-metadata case.
			return nil, false, nil
		}
		// Connection mismatch (or any other registry error) is
		// surfaced — caller's tx adoption attempt is wrong, not
		// a silent fall-through.
		return nil, false, err
	}
	return tx, true, nil
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

// DeferUntilCommit parks fn until the transaction this request rides
// commits, and reports whether it did.
//
// False means there is nothing to wait for — no `w17-tx-id` on the
// request, or an id the registry doesn't hold — and the caller should run
// fn now. That is the ordinary case: a method that opens its own
// transaction has already committed it by the time it returns.
//
// True means the caller adopted someone else's transaction. Such a method
// does NOT commit; the orchestrator does, later, after the remaining
// methods in the same transaction have run. Work that announces the
// write — an eventbus emit — must wait for that, or a rollback leaves
// subscribers acting on a mutation that never happened. On rollback fn is
// dropped.
//
// fn runs on the committing goroutine, so it must not block for long.
// Its context should be detached from the request (`context.WithoutCancel`)
// — the RPC that queued it has already returned, and its ctx may well be
// cancelled by the time the commit lands.
func DeferUntilCommit(ctx context.Context, reg CommitHook, fn func()) bool {
	if reg == nil || fn == nil {
		return false
	}
	txID := RequestTxID(ctx)
	if txID == "" {
		return false
	}
	return reg.OnCommit(txID, fn)
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
