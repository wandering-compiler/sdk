package grpcerr

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RecoverUnaryInterceptor is a server-wide panic net for generated
// gRPC servers that are NOT produced by grpcgen and therefore carry
// no per-handler `defer RecoverPanic` — chiefly the rpc gateway,
// which builds its own grpc.Server. Installed outermost in the chain
// it catches a panic in any downstream interceptor or handler,
// routes value+stack through observx (Sentry + OTel; a panic is
// always "not noise"), and returns a generic Internal — the panic
// value never reaches the client. Harmless on grpcgen handlers
// (their inner defer recovers first).
func RecoverUnaryInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			observx.ReportError(ctx, fmt.Errorf("PANIC %s: %v\n%s", info.FullMethod, r, debug.Stack()))
			err = status.Errorf(codes.Internal, "%s: internal error", info.FullMethod)
		}
	}()
	return handler(ctx, req)
}

// RecoverStreamInterceptor is the streaming counterpart of
// [RecoverUnaryInterceptor]: it recovers a panic raised on a
// streaming handler's own goroutine. (Panics on goroutines the
// handler itself spawns need their own recover at the spawn site.)
func RecoverStreamInterceptor(
	srv any,
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			observx.ReportError(ss.Context(), fmt.Errorf("PANIC %s: %v\n%s", info.FullMethod, r, debug.Stack()))
			err = status.Errorf(codes.Internal, "%s: internal error", info.FullMethod)
		}
	}()
	return handler(srv, ss)
}

// RecoverPump is the deferred recovery for a bidi-stream relay
// goroutine (the rpc gateway's client→backend / backend→client
// pumps) that reports its outcome by sending into an error channel.
// Those goroutines run off the handler goroutine, so the stream
// interceptor's recover can't see them — an uncaught panic there
// crashes the process. On panic it reports value+stack via observx
// and sends a generic Internal into errc, so the handler unblocks
// and returns cleanly instead of crashing. Emitted as the first
// statement of the pump: `defer grpcerr.RecoverPump(ctx, "Svc.M", errc)`.
func RecoverPump(ctx context.Context, label string, errc chan<- error) {
	if r := recover(); r != nil {
		observx.ReportError(ctx, fmt.Errorf("PANIC %s: %v\n%s", label, r, debug.Stack()))
		errc <- status.Errorf(codes.Internal, "%s: internal error", label)
	}
}

// RecoverPanic is a deferred-call helper for generated gRPC
// handlers (REV-031 Phase C-4 + C-6, 2026-05-09). Every
// generated handler emits:
//
//	func (s *X) Method(ctx context.Context, ...) (..., __err error) {
//	    defer grpcerr.RecoverPanic(ctx, "X.Method", &__err)
//	    ...body...
//	}
//
// On panic, it:
//
//  1. Captures the panic value + stack trace via debug.Stack().
//  2. Routes both through lib/observx — Sentry + OTel
//     active span both get the error tagged with service
//     metadata + trace_id; stderr fallback when neither
//     exporter is configured.
//  3. Assigns a generic gRPC Internal status to *errOut —
//     carries the method identity so operator log lines tie
//     back to the failing handler, but the panic VALUE never
//     escapes to the client (principle 2 — no internal-state
//     leakage).
//
// No-op when there's no panic in flight.
//
// `errOut` MUST be the address of a NAMED return value — the
// only way a deferred function can mutate the caller's return.
// Generated handlers use `__err` as the named return; tests
// can use any name.
func RecoverPanic(ctx context.Context, method string, errOut *error) {
	r := recover()
	if r == nil {
		return
	}
	stack := debug.Stack()
	// Format the panic as a Go error so observx routes it
	// uniformly with Wrap's Internal fallback. The value
	// goes through %v so both error-typed and bare
	// panic(string) cases land in one shape. ctx propagates
	// the OTel trace + service metadata so Sentry events +
	// OTel spans cross-link.
	observx.ReportError(ctx, fmt.Errorf("PANIC %s: %v\n%s", method, r, stack))
	if errOut != nil {
		*errOut = status.Errorf(codes.Internal, "%s: internal error", method)
	}
}
