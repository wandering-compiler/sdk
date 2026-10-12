package runtime

import (
	"context"
	"fmt"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// recoveryUnaryInterceptor is the process-wide gRPC panic net. It is
// installed first (outermost) in every [GRPCComponent]'s unary chain
// so it catches a panic in ANY downstream interceptor or handler —
// including gRPC servers not produced by grpcgen (the rpc gateway),
// which carry no per-handler `defer grpcerr.RecoverPanic`. For
// grpcgen handlers it is a harmless second line of defence.
//
// It is also where health calls leave the chain: see health.go.
//
// On panic it captures the value + stack, routes both through observx
// (Sentry + OTel active span, stderr fallback) tagged with the failing
// method, and returns a generic codes.Internal — the panic value never
// escapes to the client (no internal-state leakage). A panic is always
// "not noise" per quality.md §Sentry.
//
// Being outermost, it is also where the request gets its own error-tracker
// scope ([observx.WithRequestScope]), so breadcrumbs recorded while serving
// it stay with it.
func recoveryUnaryInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp any, err error) {
	ctx = observx.WithRequestScope(ctx)
	defer func() {
		if r := recover(); r != nil {
			observx.ReportError(ctx, fmt.Errorf("PANIC %s: %v\n%s", info.FullMethod, r, debug.Stack()))
			err = status.Errorf(codes.Internal, "%s: internal error", info.FullMethod)
		}
	}()
	if resp, ok, err := serveHealthUnary(ctx, req, info); ok {
		return resp, err
	}
	return handler(ctx, req)
}

// recoveryStreamInterceptor is the streaming counterpart of
// [recoveryUnaryInterceptor]: it recovers a panic raised on the
// handler's own goroutine for a streaming RPC. (Panics on goroutines
// the handler itself spawns are NOT visible here — those need their
// own recover at the spawn site.)
func recoveryStreamInterceptor(
	srv any,
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) (err error) {
	if ctx := observx.WithRequestScope(ss.Context()); ctx != ss.Context() {
		ss = scopedStream{ServerStream: ss, ctx: ctx}
	}
	defer func() {
		if r := recover(); r != nil {
			observx.ReportError(ss.Context(), fmt.Errorf("PANIC %s: %v\n%s", info.FullMethod, r, debug.Stack()))
			err = status.Errorf(codes.Internal, "%s: internal error", info.FullMethod)
		}
	}()
	if ok, err := serveHealthStream(srv, ss, info); ok {
		return err
	}
	return handler(srv, ss)
}

// scopedStream is a grpc.ServerStream whose context carries the request's
// own error-tracker scope.
type scopedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s scopedStream) Context() context.Context { return s.ctx }
