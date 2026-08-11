// Package distx is the gRPC server implementation of the
// W17DistributedTransaction service shipped in
// `proto/common/distx/distributed_tx.proto`. The server wraps
// the in-memory `txregistry.Memory` registry the storage
// binaries hold; storage handlers go through the same registry
// instance via [txregistry.AdoptTx] when they see a
// `w17-tx-id` gRPC metadata header.
//
// Single-instance default per `docs/archive/iteration-2-dql.md`
// D-iter2-dql-11. Multi-instance routing (the `conn_id` axis)
// is parked behind a future Rust grpcproxy daemon; this server
// returns an empty conn_id and ignores it on every call.
package distx

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// Server implements [distxpb.W17DistributedTransactionServer]
// over a [*txregistry.Memory] registry. One Server per binary;
// the same registry instance flows into the storage handlers
// (via their constructor's `txRegistry` parameter) so a
// caller's tx_id resolves on both surfaces.
type Server struct {
	distxpb.UnimplementedW17DistributedTransactionServer
	registry         *txregistry.Memory
	defaultTxTimeout time.Duration
}

// DefaultOrphanTimeout is the lib-default cap applied when a
// caller's `BeginRequest.tx_timeout_ms` is 0 (the proto3 zero
// value — typical for callers that don't set the field). The
// registry's Tier-2 watcher fires at this point, drains the
// orphan registry entry, and best-effort-rolls back the
// underlying *sql.Tx. Without this default, a Begin /
// Commit-or-Rollback pair where the caller drops between the
// two leaks the *sql.Tx + its pooled connection forever.
//
// Five minutes is the project-wide compromise:
//   - Long enough to outlive every realistic distributed-tx
//     flow (multi-step facade composes).
//   - Short enough that a process running for a day with one
//     leak per minute caps at ~5 stuck connections rather
//     than 1440 — the connection pool stays healthy.
//
// Operators tune via [WithDefaultTxTimeout].
const DefaultOrphanTimeout = 5 * time.Minute

// Option configures a [Server] at construction time. Using
// the option pattern keeps the constructor signature stable
// while the surface area grows (graceful-shutdown drain
// timeouts, metrics emit, etc. are reasonable future
// extensions).
type Option func(*Server)

// WithDefaultTxTimeout overrides [DefaultOrphanTimeout]. Pass
// `0` to disable the fallback entirely (callers that pass
// `tx_timeout_ms = 0` then register tx without any cleanup —
// the pre-G3-DT-01 behaviour, restored only when the
// operator explicitly opts out).
//
// The override is enforcement-only: it doesn't shorten an
// explicit caller-supplied timeout, just substitutes for an
// absent one.
func WithDefaultTxTimeout(d time.Duration) Option {
	return func(s *Server) { s.defaultTxTimeout = d }
}

// NewServer wraps reg into the gRPC service. Panics on nil
// registry — there's no useful default and a nil registry
// would surface as silent NPEs on first request.
//
// G3-DT-01: applies [DefaultOrphanTimeout] to every Begin
// whose `tx_timeout_ms` is 0; tune via
// [WithDefaultTxTimeout]. Audit closed 2026-05-05.
func NewServer(reg *txregistry.Memory, opts ...Option) *Server {
	if reg == nil {
		panic("distx.NewServer: registry is required")
	}
	s := &Server{registry: reg, defaultTxTimeout: DefaultOrphanTimeout}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Begin opens a fresh transaction on the named connection's
// *sql.DB and returns the assigned tx_id. conn_id stays empty
// — single-instance, no cross-binary routing axis (see
// D-iter2-dql-11).
//
// `req.GetConnectionName()` selects which `*sql.DB` the tx
// opens on (multi-dialect domains hold one per declared
// connection — `docs/archive/iteration-2-multidb.md` §M2-D). Empty /
// unknown connection name → `codes.InvalidArgument` with the
// registered names listed for diagnostic.
//
// `context.WithoutCancel(ctx)` decouples the tx from this
// gRPC request's lifecycle. Without it, the tx would
// auto-rollback when the gRPC framework cancels the request
// ctx after Begin returns — which is exactly the wrong
// behaviour for a distributed-tx whose whole point is to
// outlive the Begin call. Tracing / logging values on ctx
// still flow through.
func (s *Server) Begin(ctx context.Context, req *distxpb.BeginRequest) (*distxpb.BeginResponse, error) {
	if req.GetTxTimeoutMs() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "begin: tx_timeout_ms must be ≥ 0 (got %d)", req.GetTxTimeoutMs())
	}
	opts := txregistry.BeginOptions{
		ConnectionName: req.GetConnectionName(),
	}
	// Isolation is pinned here or nowhere: no engine lets a running
	// transaction change level, and a storage method called INSIDE this
	// tx cannot open one. Leaving it unset is what silently voided a
	// method's declared `tx_isolation` on the adopted path — the
	// declaration only ever reached the method's own fresh-tx branch
	// (T3-7 pass #9 D-F7). Unspecified keeps the driver default.
	if iso := isolationLevel(req.GetIsolation()); iso != sql.LevelDefault {
		opts.TxOptions = &sql.TxOptions{Isolation: iso}
	}
	switch {
	case req.GetTxTimeoutMs() > 0:
		// Caller-supplied bound — honoured verbatim.
		opts.Timeout = time.Duration(req.GetTxTimeoutMs()) * time.Millisecond
	case s.defaultTxTimeout > 0:
		// G3-DT-01 orphan guard: fall back to the server's
		// default when the caller didn't bound the tx. Without
		// this, a caller crash between Begin and Commit would
		// leak the *sql.Tx + its pooled connection forever
		// (the registry's Tier-2 watcher only spawns when
		// Timeout > 0).
		opts.Timeout = s.defaultTxTimeout
	}
	id, err := s.registry.Begin(context.WithoutCancel(ctx), opts)
	if err != nil {
		if errors.Is(err, txregistry.ErrUnknownConnection) {
			return nil, status.Errorf(codes.InvalidArgument, "begin: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "begin: %v", err)
	}
	return &distxpb.BeginResponse{TxId: id}, nil
}

// isolationLevel maps the wire enum onto the database/sql level.
// ISOLATION_UNSPECIFIED (and any value this build does not know)
// becomes sql.LevelDefault — the driver default, i.e. the behaviour
// every caller had before the field existed.
func isolationLevel(i distxpb.Isolation) sql.IsolationLevel {
	switch i {
	case distxpb.Isolation_READ_UNCOMMITTED:
		return sql.LevelReadUncommitted
	case distxpb.Isolation_READ_COMMITTED:
		return sql.LevelReadCommitted
	case distxpb.Isolation_REPEATABLE_READ:
		return sql.LevelRepeatableRead
	case distxpb.Isolation_SERIALIZABLE:
		return sql.LevelSerializable
	}
	return sql.LevelDefault
}

// Commit finalises the tx for tx_id. NotFound when the id
// doesn't resolve.
//
// ctx bounds the registry's wait for a storage handler that
// adopted this tx and is still running: closing the tx between
// two of its statements would commit half a method (T3-7 pass
// #7 C-F2). When that wait runs out the tx stays open and the
// caller gets `FailedPrecondition` — a retryable "not yet",
// distinct from `NotFound`'s "never / no longer".
func (s *Server) Commit(ctx context.Context, req *distxpb.CommitRequest) (*distxpb.CommitResponse, error) {
	if err := s.registry.Commit(ctx, req.GetTxId()); err != nil {
		if errors.Is(err, txregistry.ErrUnknownTxID) {
			// The registry's message carries the id AND, when it still
			// remembers, what became of the transaction — most usefully
			// that a failed call inside it triggered the auto-rollback.
			// Formatting the id here instead would throw that away and
			// leave the caller with a symptom.
			return nil, status.Errorf(codes.NotFound, "commit: %v", err)
		}
		if errors.Is(err, txregistry.ErrTxBusy) {
			return nil, status.Errorf(codes.FailedPrecondition, "commit: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "commit: %v", err)
	}
	return &distxpb.CommitResponse{}, nil
}

// Rollback discards the tx for tx_id. NotFound when the id
// doesn't resolve, FailedPrecondition when an adopted handler
// is still running on it (see [Server.Commit]).
func (s *Server) Rollback(ctx context.Context, req *distxpb.RollbackRequest) (*distxpb.RollbackResponse, error) {
	if err := s.registry.Rollback(ctx, req.GetTxId()); err != nil {
		if errors.Is(err, txregistry.ErrUnknownTxID) {
			return nil, status.Errorf(codes.NotFound, "rollback: %v", err)
		}
		if errors.Is(err, txregistry.ErrTxBusy) {
			return nil, status.Errorf(codes.FailedPrecondition, "rollback: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "rollback: %v", err)
	}
	return &distxpb.RollbackResponse{}, nil
}
