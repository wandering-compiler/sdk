package inprocgrpc_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// mustPanic runs fn and returns the panic message, failing when fn returns.
func mustPanic(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic; want registration refused")
		}
		msg, _ = r.(string)
	}()
	fn()
	return ""
}

// A wire *grpc.Server refuses a service registered twice. In-process the
// second registration used to replace the first silently, so whichever tier
// or plugin registered last served the methods.
func TestConn_RegisterService_DuplicateIsRefused(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(echoServiceDesc(), &echoServer{})
	msg := mustPanic(t, func() { conn.RegisterService(echoServiceDesc(), &echoServer{}) })
	if !strings.Contains(msg, "duplicate service registration") || !strings.Contains(msg, "test.Echo") {
		t.Errorf("panic = %q, want it to name the duplicate service", msg)
	}
}

// An impl that does not implement the descriptor's handler type is refused
// at registration, as on the wire, instead of panicking on the first call.
func TestConn_RegisterService_WrongImplIsRefused(t *testing.T) {
	type echoHandler interface {
		Echo(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
	}
	sd := echoServiceDesc()
	sd.HandlerType = (*echoHandler)(nil)
	conn := inprocgrpc.New()
	msg := mustPanic(t, func() { conn.RegisterService(sd, struct{}{}) })
	if !strings.Contains(msg, "does not satisfy") {
		t.Errorf("panic = %q, want a handler-type refusal", msg)
	}
	// The right impl still registers.
	inprocgrpc.New().RegisterService(sd, &echoServer{})
}

// Interceptor options add to one chain in the order given. Mixing the
// singular and the slice option used to keep only the last one, dropping
// the earlier interceptors (rollback, emit, auth) without an error.
func TestConn_UnaryInterceptorOptionsAppend(t *testing.T) {
	var order []string
	mk := func(name string) grpc.UnaryServerInterceptor {
		return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			order = append(order, name)
			return h(ctx, req)
		}
	}
	conn := inprocgrpc.New(
		inprocgrpc.WithUnaryInterceptors(mk("a"), mk("b")),
		inprocgrpc.WithUnaryInterceptor(mk("c")),
		inprocgrpc.WithUnaryInterceptor(nil),
		inprocgrpc.WithUnaryInterceptors(mk("d")),
	)
	conn.RegisterService(echoServiceDesc(), &echoServer{})
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := strings.Join(order, ","); got != "a,b,c,d" {
		t.Errorf("interceptors ran %q, want a,b,c,d", got)
	}
}

func TestConn_StreamInterceptorOptionsAppend(t *testing.T) {
	var order []string
	mk := func(name string) grpc.StreamServerInterceptor {
		return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			order = append(order, name)
			return h(srv, ss)
		}
	}
	conn := inprocgrpc.New(
		inprocgrpc.WithStreamInterceptor(mk("a")),
		inprocgrpc.WithStreamInterceptors(mk("b"), mk("c")),
		inprocgrpc.WithStreamInterceptor(mk("d")),
	)
	conn.RegisterService(countServiceDesc(), &countServer{})
	cs, err := conn.NewStream(context.Background(),
		&grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if _, err := drain(t, cs, 1); err == nil {
		t.Fatal("want a terminal error")
	}
	if got := strings.Join(order, ","); got != "a,b,c,d" {
		t.Errorf("stream interceptors ran %q, want a,b,c,d", got)
	}
}

// blockingStreamDesc's handler waits for its context and returns whatever
// error ret gives it.
func blockingStreamDesc(ret func(grpc.ServerStream) error) *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName: "Count", ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error { return ret(stream) },
		}},
	}
}

// Context errors on a stream are status errors with the wire's codes, so
// status.Code answers Canceled / DeadlineExceeded rather than Unknown.
func TestConn_Stream_ContextErrorsAreStatusErrors(t *testing.T) {
	t.Run("caller cancel on RecvMsg", func(t *testing.T) {
		conn := inprocgrpc.New()
		conn.RegisterService(blockingStreamDesc(func(s grpc.ServerStream) error {
			<-s.Context().Done()
			return s.Context().Err()
		}), nil)
		ctx, cancel := context.WithCancel(context.Background())
		cs, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		err = cs.RecvMsg(new(wrapperspb.Int32Value))
		if code := status.Code(err); code != codes.Canceled {
			t.Errorf("RecvMsg = %v (code %v), want Canceled", err, code)
		}
		if err := cs.SendMsg(wrapperspb.Int32(1)); status.Code(err) != codes.Canceled {
			t.Errorf("SendMsg after cancel = %v, want Canceled", err)
		}
		if _, err := cs.Header(); status.Code(err) != codes.Canceled {
			t.Errorf("Header after cancel = %v, want Canceled", err)
		}
	})

	t.Run("caller deadline on RecvMsg", func(t *testing.T) {
		conn := inprocgrpc.New()
		conn.RegisterService(blockingStreamDesc(func(s grpc.ServerStream) error {
			<-s.Context().Done()
			return s.Context().Err()
		}), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		cs, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
		if err != nil {
			t.Fatal(err)
		}
		err = cs.RecvMsg(new(wrapperspb.Int32Value))
		if code := status.Code(err); code != codes.DeadlineExceeded {
			t.Errorf("RecvMsg = %v (code %v), want DeadlineExceeded", err, code)
		}
	})

	t.Run("handler returns a bare context error", func(t *testing.T) {
		conn := inprocgrpc.New()
		conn.RegisterService(blockingStreamDesc(func(grpc.ServerStream) error {
			return context.DeadlineExceeded
		}), nil)
		cs, err := conn.NewStream(context.Background(), &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
		if err != nil {
			t.Fatal(err)
		}
		err = cs.RecvMsg(new(wrapperspb.Int32Value))
		if code := status.Code(err); code != codes.DeadlineExceeded {
			t.Errorf("final status = %v (code %v), want DeadlineExceeded", err, code)
		}
	})

	t.Run("handler RecvMsg after its context ends", func(t *testing.T) {
		got := make(chan error, 1)
		conn := inprocgrpc.New()
		conn.RegisterService(blockingStreamDesc(func(s grpc.ServerStream) error {
			<-s.Context().Done()
			got <- s.RecvMsg(new(wrapperspb.Int32Value))
			return nil
		}), nil)
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case err := <-got:
			if status.Code(err) != codes.Canceled {
				t.Errorf("server RecvMsg = %v, want Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not observe the cancel")
		}
	})

	t.Run("a non-context handler error is untouched", func(t *testing.T) {
		boom := errors.New("boom")
		conn := inprocgrpc.New()
		conn.RegisterService(blockingStreamDesc(func(grpc.ServerStream) error { return boom }), nil)
		cs, err := conn.NewStream(context.Background(), &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
		if err != nil {
			t.Fatal(err)
		}
		if err := cs.RecvMsg(new(wrapperspb.Int32Value)); err != boom {
			t.Errorf("final error = %v, want the handler's own error", err)
		}
	})
}

// The unary twin: a handler that returns a bare context error reports the
// wire's code.
func TestInvoke_HandlerContextErrorIsStatusError(t *testing.T) {
	conn := inprocgrpc.New(inprocgrpc.WithUnaryInterceptor(
		func(context.Context, any, *grpc.UnaryServerInfo, grpc.UnaryHandler) (any, error) {
			return nil, context.Canceled
		}))
	conn.RegisterService(echoServiceDesc(), &echoServer{})
	err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("x"), new(wrapperspb.StringValue))
	if code := status.Code(err); code != codes.Canceled {
		t.Errorf("Invoke = %v (code %v), want Canceled", err, code)
	}
}
