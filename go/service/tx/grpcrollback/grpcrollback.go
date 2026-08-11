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
//
//   - Doesn't track per-RPC tx ownership. It rollbacks
//     whatever tx the caller threaded via `w17-tx-id`,
//     regardless of whether the handler opened a fresh tx or
//     adopted the caller's. Single-conn fresh-tx handlers
//     still own their own Rollback path (deferred inside the
//     handler body); calling Rollback on a tx the registry
//     no longer holds (because the handler's deferred
//     rollback already drained it via take()) returns
//     ErrUnknownTxID, which we silently ignore.
//
//     ⚠️ That "already drained" reasoning stopped being the
//     whole story when take() grew an adoption lease: a take
//     that gives up now leaves the entry LIVE and returns
//     ErrTxBusy. A rollback fired on that entry destroys a
//     transaction its owner was still trying to commit, which
//     is why control-plane methods are filtered out below.
//
//   - Doesn't fire on streaming handlers. Streaming + auto-
//     rollback is a future extension; today only unary RPCs
//     are wrapped.
//
//   - Doesn't mask the original handler error. The Rollback
//     error (if any) is dropped; the handler's error
//     propagates to the caller verbatim.
package grpcrollback

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// TxRoller exposes the tx-rollback method the interceptor
// needs. Defined here (not on [txregistry.Registry], which is
// intentionally minimal — read-only `LookupTx`) so the
// interceptor accepts an explicit small interface without
// pulling in Begin / Commit. `*txregistry.Memory` satisfies
// it via its `Rollback(ctx, txID) error` method — ctx bounds
// the registry's wait for any handler still holding the tx, so
// the auto-rollback of a FAILED sibling can never close the
// transaction between two statements of one that is still
// running (T3-7 pass #7 C-F2).
type TxRoller interface {
	Rollback(ctx context.Context, txID string) error
}

// CausedRoller is the optional slice of a [TxRoller] that can record
// WHY a transaction was discarded. `*txregistry.Memory` implements it.
//
// Optional rather than part of [TxRoller] so an existing third-party
// roller keeps compiling — such a roller simply loses the diagnostic,
// never the rollback.
//
// It exists because this interceptor destroys transactions on behalf of
// a caller that did not ask for it, and the caller finds out somewhere
// else entirely: its own Commit, several calls later, answering
// "unknown tx_id". That message describes the state of the registry
// rather than the reason for it, and a caller reading it reasonably
// concludes its transaction plumbing is broken — the one thing that is
// NOT happening. Handing the reason over here is what lets the later
// surface name this call instead.
type CausedRoller interface {
	TxRoller
	RollbackCaused(ctx context.Context, txID, cause string) error
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
// isControlPlane reports whether a method is the distributed transaction's own
// lifecycle surface (Begin / Commit / Rollback) rather than work performed
// inside a transaction. Matched on the service prefix so a new control-plane
// method is covered the day it is added rather than the day it is noticed.
func isControlPlane(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/w17.common.distx.W17DistributedTransaction/")
}

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
		// The transaction's OWN lifecycle calls are not work done inside it.
		//
		// Commit carries `w17-tx-id` like any participant (the Begin-derived
		// context has it regardless of what the client attaches), so a Commit
		// that comes back retryable — ErrTxBusy, when a handler still holds
		// the adoption lease — looked to this interceptor like a failed
		// participant and got the transaction rolled back. The coordinator
		// asked to commit, was told to retry, and the middleware converted
		// its intent into a destroy; the retry then sees NotFound, which is
		// indistinguishable from "already committed". A failed Begin on a
		// context derived from another transaction's flow had the same shape.
		if isControlPlane(info.FullMethod) {
			return resp, err
		}
		// Best-effort rollback. Errors (typically
		// ErrUnknownTxID) are intentionally ignored — we don't
		// want a registry-side drain race to mask the handler's
		// real error.
		//
		// The cause travels with it when the roller can hold one, so
		// the coordinator's Commit can name THIS call. Note what it
		// carries: the method and the status code, never the handler's
		// error string — that has not been through the error layer's
		// sanitisation and must not reach a client via the back door of
		// a diagnostic (no schema / table / column leakage).
		if cr, ok := roller.(CausedRoller); ok {
			_ = cr.RollbackCaused(ctx, txID, fmt.Sprintf(
				"a call to %s inside it failed with %s, and any failure inside a transaction discards it",
				info.FullMethod, status.Code(err)))
		} else {
			_ = roller.Rollback(ctx, txID)
		}
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
