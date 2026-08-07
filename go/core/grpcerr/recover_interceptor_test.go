package grpcerr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeServerStream is a minimal grpc.ServerStream whose only
// load-bearing method is Context() — the recover interceptor
// reads it to route the panic report.
type fakeServerStream struct {
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context     { return f.ctx }
func (f *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeServerStream) SetTrailer(metadata.MD)       {}
func (f *fakeServerStream) SendMsg(m any) error          { return nil }
func (f *fakeServerStream) RecvMsg(m any) error          { return nil }

// INVARIANT: a panic inside a unary handler is recovered, reported
// to the internal log, and converted into a generic Internal status
// that carries the method identity but NOT the panic value.
func TestRecoverUnaryInterceptor_RecoversPanic(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/svc.User/Create"}
	handler := func(ctx context.Context, req any) (any, error) {
		panic("boom secret state")
	}
	var resp any
	var err error
	logged := withCapturedLog(t, func() {
		resp, err = RecoverUnaryInterceptor(context.Background(), "req", info, handler)
	})
	if resp != nil {
		t.Errorf("resp should be nil on panic, got %v", resp)
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal {
		t.Fatalf("want Internal status, got %v (ok=%v)", err, ok)
	}
	if !strings.Contains(st.Message(), "/svc.User/Create") {
		t.Errorf("message must carry method identity: %q", st.Message())
	}
	if strings.Contains(st.Message(), "boom") {
		t.Errorf("panic value leaked to client: %q", st.Message())
	}
	if !strings.Contains(logged, "PANIC /svc.User/Create") || !strings.Contains(logged, "boom") {
		t.Errorf("internal log should carry panic value + method: %q", logged)
	}
}

// INVARIANT: the happy path passes response + error through unchanged
// and reports nothing.
func TestRecoverUnaryInterceptor_PassThrough(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/svc.User/Get"}
	sentinel := errors.New("handler error")
	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", sentinel
	}
	logged := withCapturedLog(t, func() {
		resp, err := RecoverUnaryInterceptor(context.Background(), "req", info, handler)
		if resp != "ok" || !errors.Is(err, sentinel) {
			t.Errorf("pass-through failed: resp=%v err=%v", resp, err)
		}
	})
	if logged != "" {
		t.Errorf("no panic should produce no log: %q", logged)
	}
}

// INVARIANT: a panic on the streaming handler goroutine is recovered
// into a generic Internal status routed through the stream's context.
func TestRecoverStreamInterceptor_RecoversPanic(t *testing.T) {
	info := &grpc.StreamServerInfo{FullMethod: "/svc.User/Watch"}
	ss := &fakeServerStream{ctx: context.Background()}
	handler := func(srv any, stream grpc.ServerStream) error {
		panic(errors.New("stream blew up"))
	}
	var err error
	logged := withCapturedLog(t, func() {
		err = RecoverStreamInterceptor(nil, ss, info, handler)
	})
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Internal {
		t.Fatalf("want Internal status, got %v (ok=%v)", err, ok)
	}
	if !strings.Contains(st.Message(), "/svc.User/Watch") {
		t.Errorf("message must carry method identity: %q", st.Message())
	}
	if !strings.Contains(logged, "PANIC /svc.User/Watch") {
		t.Errorf("internal log should carry PANIC line: %q", logged)
	}
}

// INVARIANT: the streaming happy path returns the handler's error
// verbatim with no recovery/logging.
func TestRecoverStreamInterceptor_PassThrough(t *testing.T) {
	info := &grpc.StreamServerInfo{FullMethod: "/svc.User/Watch"}
	ss := &fakeServerStream{ctx: context.Background()}
	sentinel := errors.New("stream done")
	handler := func(srv any, stream grpc.ServerStream) error {
		return sentinel
	}
	logged := withCapturedLog(t, func() {
		if err := RecoverStreamInterceptor(nil, ss, info, handler); !errors.Is(err, sentinel) {
			t.Errorf("pass-through failed: %v", err)
		}
	})
	if logged != "" {
		t.Errorf("no panic should produce no log: %q", logged)
	}
}

// INVARIANT: a panic in a relay-pump goroutine is recovered, reported,
// and a generic Internal status is sent into the error channel so the
// owning handler unblocks instead of crashing the process.
func TestRecoverPump_RecoversPanicIntoChannel(t *testing.T) {
	errc := make(chan error, 1)
	logged := withCapturedLog(t, func() {
		func() {
			defer RecoverPump(context.Background(), "Svc.Relay", errc)
			panic("pump panic")
		}()
	})
	select {
	case err := <-errc:
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.Internal {
			t.Fatalf("want Internal in channel, got %v (ok=%v)", err, ok)
		}
		if !strings.Contains(st.Message(), "Svc.Relay") {
			t.Errorf("message must carry label: %q", st.Message())
		}
	default:
		t.Fatal("RecoverPump did not send into errc on panic")
	}
	if !strings.Contains(logged, "PANIC Svc.Relay") {
		t.Errorf("internal log should carry PANIC line: %q", logged)
	}
}

// INVARIANT: with no panic in flight RecoverPump is a no-op — nothing
// is sent into the channel and nothing is logged.
func TestRecoverPump_NoPanicNoOp(t *testing.T) {
	errc := make(chan error, 1)
	logged := withCapturedLog(t, func() {
		func() {
			defer RecoverPump(context.Background(), "Svc.Relay", errc)
			// no panic
		}()
	})
	select {
	case err := <-errc:
		t.Fatalf("RecoverPump sent on no-panic: %v", err)
	default:
	}
	if logged != "" {
		t.Errorf("no panic should produce no log: %q", logged)
	}
}
