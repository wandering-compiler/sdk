// Package grpcrollback provides a unary gRPC server
// interceptor that auto-rollbacks any caller-supplied
// distributed transaction when the wrapped handler returns a
// non-nil error.
//
// Wiring (per-binary, in `genmain/main.tmpl`):
//
//	registry := txregistry.NewMemory(map[string]*sql.DB{...})
//	srv := grpc.NewServer(grpc.UnaryInterceptor(grpcrollback.Interceptor(registry)))
//
// Per `docs/archive/iteration-2-dql.md` D-iter2-dql-4 + the 2026-05-03
// revision: auto-rollback is cross-cutting middleware, not a
// per-handler generator output. The interceptor wraps EVERY
// unary handler on the binary's gRPC server (Layer-1 storage
// handlers today; any Layer-2 handler the developer adds in
// the same binary picks it up uniformly).
//
// What the interceptor does NOT do:
//   - Doesn't track per-RPC tx ownership. It rollbacks
//     whatever tx the caller threaded via `w17-tx-id`,
//     regardless of whether the handler opened a fresh tx or
//     adopted the caller's. Single-conn fresh-tx handlers
//     still own their own Rollback path (deferred inside the
//     handler body); calling Rollback on a tx the registry
//     no longer holds (because the handler's deferred
//     rollback already drained it via take()) returns
//     ErrUnknownTxID, which we silently ignore.
//   - Doesn't fire on streaming handlers. Streaming + auto-
//     rollback is a future extension; today only unary RPCs
//     are wrapped.
//   - Doesn't mask the original handler error. The Rollback
//     error (if any) is dropped; the handler's error
//     propagates to the caller verbatim.
package grpcrollback

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// TxRoller exposes the tx-rollback method the interceptor
// needs. Defined here (not on [txregistry.Registry], which is
// intentionally minimal — read-only `LookupTx`) so the
// interceptor accepts an explicit small interface without
// pulling in Begin / Commit. `*txregistry.Memory` satisfies
// it via the existing `Rollback(txID string) error` method.
type TxRoller interface {
	Rollback(txID string) error
}

// Interceptor returns a gRPC unary server interceptor that
// auto-rollbacks the caller-supplied distributed transaction
// when the wrapped handler returns a non-nil error.
//
// Behaviour:
//
//   - Calls the wrapped handler.
//   - If the handler returns nil error → pass through.
//   - If the handler returns an error AND the incoming gRPC
//     metadata carries a non-empty `w17-tx-id` AND `roller`
//     is non-nil → calls `roller.Rollback(txID)` then
//     propagates the original error.
//   - The Rollback error (typically [txregistry.ErrUnknownTxID]
//     when the registry has already drained the entry — user
//     Commit, Tier-2 deadline watcher, handler-side deferred
//     rollback) is silently ignored. We don't mask the
//     handler's error.
//
// Pass nil `roller` to disable auto-rollback (the interceptor
// becomes a no-op pass-through). Useful for binaries without
// a tx registry — e.g. query-only services that never adopt
// caller transactions.
func Interceptor(roller TxRoller) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}
		if roller == nil {
			return resp, err
		}
		txID := readTxID(ctx)
		if txID == "" {
			return resp, err
		}
		// Best-effort rollback. Errors (typically
		// ErrUnknownTxID) are intentionally ignored — we don't
		// want a registry-side drain race to mask the handler's
		// real error.
		_ = roller.Rollback(txID)
		return resp, err
	}
}

// readTxID extracts the `w17-tx-id` value from the incoming
// gRPC metadata, returning empty when absent / empty / no
// metadata at all.
func readTxID(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(txregistry.HeaderName)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
