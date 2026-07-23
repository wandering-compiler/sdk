package inprocgrpc_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const echoMethod = "/test.Echo/Echo"

// echoServer is a stand-in for a generated gRPC server impl: it echoes
// the request value back (prefixed) and records the metadata it saw as
// INCOMING (the wire contract the in-process bridge must reproduce).
type echoServer struct {
	sawIncoming string
}

func (s *echoServer) Echo(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("x-w17-user"); len(v) > 0 {
			s.sawIncoming = v[0]
		}
	}
	return wrapperspb.String("echo:" + in.GetValue()), nil
}

// echoServiceDesc hand-rolls the ServiceDesc a generated
// RegisterXxxServer would pass — one unary method whose Handler honours
// the grpc methodHandler contract (allocate request, dec it, run the
// interceptor, call the impl).
func echoServiceDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "test.Echo",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Echo",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.StringValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				impl := srv.(*echoServer)
				if interceptor == nil {
					return impl.Echo(ctx, in)
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: echoMethod}
				return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
					return impl.Echo(ctx, req.(*wrapperspb.StringValue))
				})
			},
		}},
	}
}

func TestConn_DirectDispatch_MetadataAndInterceptor(t *testing.T) {
	var interceptorRan bool
	conn := inprocgrpc.New(inprocgrpc.WithUnaryInterceptor(
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			interceptorRan = true
			if info.FullMethod != echoMethod {
				t.Errorf("interceptor FullMethod = %q, want %q", info.FullMethod, echoMethod)
			}
			return handler(ctx, req)
		}))

	srv := &echoServer{}
	conn.RegisterService(echoServiceDesc(), srv)

	// Caller sets metadata as OUTGOING (the wire convention); the bridge
	// must flip it to INCOMING for the handler.
	ctx := metadata.NewOutgoingContext(context.Background(),
		metadata.Pairs("x-w17-user", "alice"))

	in := wrapperspb.String("hi")
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(ctx, echoMethod, in, out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if got, want := out.GetValue(), "echo:hi"; got != want {
		t.Errorf("response = %q, want %q (direct dispatch failed)", got, want)
	}
	if !interceptorRan {
		t.Errorf("unary interceptor did not run")
	}
	if srv.sawIncoming != "alice" {
		t.Errorf("handler saw incoming metadata %q, want %q (outgoing→incoming flip failed)", srv.sawIncoming, "alice")
	}

	// args must not be mutated by the handler's decode (the bridge hands
	// the handler a copy).
	if in.GetValue() != "hi" {
		t.Errorf("caller's request value was mutated to %q", in.GetValue())
	}
}

func TestConn_UnknownMethod_Unimplemented(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(echoServiceDesc(), &echoServer{})

	out := new(wrapperspb.StringValue)
	err := conn.Invoke(context.Background(), "/test.Echo/Missing", wrapperspb.String("x"), out)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unknown method: got %v, want Unimplemented", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "Missing") {
		t.Errorf("error should name the missing method: %v", err)
	}
}

// An UNREGISTERED streaming method is Unimplemented (mirrors the unary
// unknown-method behaviour) — a registered one is dispatched, see below.
func TestConn_Streaming_Unregistered_Unimplemented(t *testing.T) {
	conn := inprocgrpc.New()
	_, err := conn.NewStream(context.Background(), &grpc.StreamDesc{}, "/test.Counter/Count")
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("streaming: got %v, want Unimplemented", err)
	}
}

// countServer is a stand-in for a generated server-streaming impl: given a
// count N it streams 0..N-1 back, records the incoming metadata it saw,
// and returns finalErr after the last message (to exercise trailer-error
// propagation).
type countServer struct {
	sawIncoming string
	finalErr    error
}

// countServiceDesc hand-rolls the ServiceDesc a generated
// RegisterXxxServer would pass for ONE server-streaming method. The
// Handler mirrors grpc-gen: allocate the request, RecvMsg it, then call
// the impl with the stream (here inlined).
func countServiceDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName:    "Count",
			ServerStreams: true,
			Handler: func(srv any, stream grpc.ServerStream) error {
				impl := srv.(*countServer)
				req := new(wrapperspb.Int32Value)
				if err := stream.RecvMsg(req); err != nil {
					return err
				}
				if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
					if v := md.Get("x-w17-user"); len(v) > 0 {
						impl.sawIncoming = v[0]
					}
				}
				for i := int32(0); i < req.GetValue(); i++ {
					if err := stream.SendMsg(wrapperspb.Int32(i)); err != nil {
						return err
					}
				}
				return impl.finalErr
			},
		}},
	}
}

const countMethod = "/test.Counter/Count"

// drain runs the generated server-streaming client pattern (SendMsg the
// single request, CloseSend, RecvMsg until a terminal error) and returns
// the received values + the terminal error (io.EOF on clean completion).
func drain(t *testing.T, cs grpc.ClientStream, n int32) ([]int32, error) {
	t.Helper()
	if err := cs.SendMsg(wrapperspb.Int32(n)); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if err := cs.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	var got []int32
	for {
		out := new(wrapperspb.Int32Value)
		err := cs.RecvMsg(out)
		if err != nil {
			return got, err
		}
		got = append(got, out.GetValue())
	}
}

// Happy path: a registered server-streaming method streams all responses
// through the in-process channel pair, terminating in io.EOF, and the
// handler sees the caller's OUTGOING metadata as INCOMING (the wire flip).
func TestConn_ServerStreaming_HappyPath(t *testing.T) {
	srv := &countServer{}
	conn := inprocgrpc.New()
	conn.RegisterService(countServiceDesc(), srv)

	ctx := metadata.NewOutgoingContext(context.Background(),
		metadata.Pairs("x-w17-user", "alice"))
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	got, err := drain(t, cs, 3)
	if err != io.EOF {
		t.Fatalf("terminal err: got %v, want io.EOF", err)
	}
	want := []int32{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if srv.sawIncoming != "alice" {
		t.Fatalf("handler incoming metadata: got %q, want %q", srv.sawIncoming, "alice")
	}
}

// The handler's terminal (trailer) error reaches the caller as the final
// RecvMsg error, AFTER the already-sent messages — not io.EOF.
func TestConn_Streaming_TrailerErrorPropagates(t *testing.T) {
	srv := &countServer{finalErr: status.Error(codes.FailedPrecondition, "boom")}
	conn := inprocgrpc.New()
	conn.RegisterService(countServiceDesc(), srv)

	cs, err := conn.NewStream(context.Background(), &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	got, err := drain(t, cs, 2)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("terminal err: got %v, want FailedPrecondition", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages before the error, got %v", got)
	}
}

// A caller that keeps sending after the handler returned early must not
// hang. The handler exits immediately (without draining c2s), which cancels
// the server context but NOT the caller's parent context; SendMsg has to
// observe the server side ending and return an error instead of blocking
// forever on the unbuffered c2s send whose reader is gone.
func TestConn_Streaming_SendAfterServerDoneDoesNotHang(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName: "Count", ClientStreams: true,
			// Returns immediately without reading any client message.
			Handler: func(any, grpc.ServerStream) error {
				return status.Error(codes.FailedPrecondition, "early out")
			},
		}},
	}, nil)

	cs, err := conn.NewStream(context.Background(),
		&grpc.StreamDesc{StreamName: "Count", ClientStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Keep sending; one of these must return an error once the handler
	// goroutine has exited rather than wedging the caller forever.
	done := make(chan error, 1)
	go func() {
		for {
			if err := cs.SendMsg(wrapperspb.Int32(1)); err != nil {
				done <- err
				return
			}
		}
	}()

	select {
	case <-done:
		// unblocked with an error — correct.
	case <-time.After(2 * time.Second):
		t.Fatal("SendMsg hung after the handler returned early")
	}
}

// A panicking handler is recovered into an Internal error rather than
// crashing the process — parity with the wire server's recover interceptor.
func TestConn_Streaming_PanicRecovered(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName: "Count", ServerStreams: true,
			Handler: func(any, grpc.ServerStream) error { panic("kaboom") },
		}},
	}, nil)

	cs, err := conn.NewStream(context.Background(), &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	out := new(wrapperspb.Int32Value)
	if err := cs.RecvMsg(out); status.Code(err) != codes.Internal {
		t.Fatalf("panic: got %v, want Internal", err)
	}
}

// Caller cancellation unblocks a waiting RecvMsg (and the handler's
// context is cancelled too, so a blocked handler stops).
func TestConn_Streaming_CallerCancel(t *testing.T) {
	conn := inprocgrpc.New()
	handlerDone := make(chan struct{})
	conn.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName: "Count", ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				<-stream.Context().Done() // blocks until caller cancels
				close(handlerDone)
				return stream.Context().Err()
			},
		}},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	cancel()
	out := new(wrapperspb.Int32Value)
	if err := cs.RecvMsg(out); err == nil {
		t.Fatalf("RecvMsg after cancel: got nil, want a cancellation error")
	}
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not observe context cancellation")
	}
}

func TestConn_NoInterceptor_StillDispatches(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(echoServiceDesc(), &echoServer{})
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.GetValue() != "echo:x" {
		t.Errorf("response = %q, want %q", out.GetValue(), "echo:x")
	}
}

// TestConn_WithUnaryInterceptors_ChainOrder verifies the chain runs
// outermost-first (grpc-go semantics): a tier hands its
// Resources.Interceptors() slice straight through and each wraps the
// next, the handler innermost.
func TestConn_WithUnaryInterceptors_ChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) grpc.UnaryServerInterceptor {
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			order = append(order, name+":before")
			resp, err := handler(ctx, req)
			order = append(order, name+":after")
			return resp, err
		}
	}
	conn := inprocgrpc.New(inprocgrpc.WithUnaryInterceptors(mk("a"), mk("b")))
	conn.RegisterService(echoServiceDesc(), &echoServer{})

	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.GetValue() != "echo:x" {
		t.Errorf("response = %q, want %q", out.GetValue(), "echo:x")
	}
	// a wraps b wraps handler: a:before, b:before, <handler>, b:after, a:after.
	want := []string{"a:before", "b:before", "b:after", "a:after"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("chain order = %v, want %v", order, want)
	}
}

// TestConn_WithUnaryInterceptors_Empty installs no interceptor for an
// empty slice (a tier with no server-side chain).
func TestConn_WithUnaryInterceptors_Empty(t *testing.T) {
	conn := inprocgrpc.New(inprocgrpc.WithUnaryInterceptors())
	conn.RegisterService(echoServiceDesc(), &echoServer{})
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("x"), out); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.GetValue() != "echo:x" {
		t.Errorf("response = %q, want %q", out.GetValue(), "echo:x")
	}
}
