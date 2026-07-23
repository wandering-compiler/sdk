package runtime

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A panicking unary handler is recovered into a generic Internal
// status; the panic value never reaches the returned error.
func TestRecoveryUnaryInterceptor_RecoversPanic(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/app.Storage/Create"}
	handler := func(context.Context, any) (any, error) {
		panic("secret internal state: password=hunter2")
	}

	resp, err := recoveryUnaryInterceptor(context.Background(), nil, info, handler)
	if resp != nil {
		t.Fatalf("resp = %v, want nil after recovered panic", resp)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if !strings.Contains(err.Error(), info.FullMethod) {
		t.Fatalf("error %q should name the failing method", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("panic value leaked to client error: %q", err)
	}
}

// A non-panicking handler passes through untouched.
func TestRecoveryUnaryInterceptor_PassThrough(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/app.Storage/Get"}
	handler := func(context.Context, any) (any, error) { return "ok", nil }
	resp, err := recoveryUnaryInterceptor(context.Background(), nil, info, handler)
	if err != nil || resp != "ok" {
		t.Fatalf("pass-through = (%v, %v), want (ok, nil)", resp, err)
	}
}

// fakeStream is a minimal grpc.ServerStream carrying a context.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s fakeStream) Context() context.Context { return s.ctx }

// A panicking stream handler is recovered into Internal.
func TestRecoveryStreamInterceptor_RecoversPanic(t *testing.T) {
	info := &grpc.StreamServerInfo{FullMethod: "/app.Storage/Watch"}
	handler := func(any, grpc.ServerStream) error { panic("boom") }
	err := recoveryStreamInterceptor(nil, fakeStream{ctx: context.Background()}, info, handler)
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}
