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
// On panic it captures the value + stack, routes both through observx
// (Sentry + OTel active span, stderr fallback) tagged with the failing
// method, and returns a generic codes.Internal — the panic value never
// escapes to the client (no internal-state leakage). A panic is always
// "not noise" per quality.md §Sentry.
func recoveryUnaryInterceptor(
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
	defer func() {
		if r := recover(); r != nil {
			observx.ReportError(ss.Context(), fmt.Errorf("PANIC %s: %v\n%s", info.FullMethod, r, debug.Stack()))
			err = status.Errorf(codes.Internal, "%s: internal error", info.FullMethod)
		}
	}()
	return handler(srv, ss)
}
