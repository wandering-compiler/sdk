package inprocgrpc_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// TestConn_StreamInterceptor_Runs pins C-F7. NewStream never consulted an
// interceptor chain and the Conn had no stream-interceptor option AT ALL,
// so the in-process transport could not run one even in principle — while a
// standalone bundle installs grpc.ChainStreamInterceptor on its
// *grpc.Server. The first stream interceptor a tier grows would therefore
// have applied on the wire and silently not in a composed binary, and a
// green build would have proved nothing (the Unimplemented-embed trap's
// cousin).
func TestConn_StreamInterceptor_Runs(t *testing.T) {
	var sawMethod string
	var sawServerStream bool
	conn := inprocgrpc.New(inprocgrpc.WithStreamInterceptor(
		func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			sawMethod = info.FullMethod
			sawServerStream = info.IsServerStream
			return handler(srv, ss)
		}))
	conn.RegisterService(countServiceDesc(), &countServer{})

	cs, err := conn.NewStream(context.Background(),
		&grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if _, err := drain(t, cs, 2); err == nil {
		t.Fatalf("expected the stream to terminate")
	}

	if sawMethod != countMethod {
		t.Errorf("stream interceptor did not run in-process (FullMethod = %q, want %q): the same chain "+
			"runs on the wire, so a composed binary would silently skip it", sawMethod, countMethod)
	}
	if !sawServerStream {
		t.Errorf("StreamServerInfo.IsServerStream = false: the interceptor cannot tell what kind of " +
			"stream it is wrapping")
	}
}

// The chain runs outermost-first, matching grpc-go's
// ChainStreamInterceptor — so a tier can hand its slice straight through
// and get the same order it gets on the wire.
func TestConn_WithStreamInterceptors_ChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) grpc.StreamServerInterceptor {
		return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			order = append(order, name+":before")
			err := handler(srv, ss)
			order = append(order, name+":after")
			return err
		}
	}
	conn := inprocgrpc.New(inprocgrpc.WithStreamInterceptors(mk("a"), mk("b")))
	conn.RegisterService(countServiceDesc(), &countServer{})

	cs, err := conn.NewStream(context.Background(),
		&grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if _, err := drain(t, cs, 1); err == nil {
		t.Fatalf("expected the stream to terminate")
	}
	want := "a:before,b:before,b:after,a:after"
	if strings.Join(order, ",") != want {
		t.Errorf("chain order = %v, want %v", order, want)
	}
}

// An interceptor that fails the call must surface as the stream's terminal
// status, not be swallowed — otherwise a guard could "run" and change
// nothing.
func TestConn_StreamInterceptor_RejectionSurfaces(t *testing.T) {
	conn := inprocgrpc.New(inprocgrpc.WithStreamInterceptors(
		func(any, grpc.ServerStream, *grpc.StreamServerInfo, grpc.StreamHandler) error {
			return context.Canceled
		}))
	handlerRan := false
	conn.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName: "Count", ServerStreams: true,
			Handler: func(any, grpc.ServerStream) error { handlerRan = true; return nil },
		}},
	}, nil)

	cs, err := conn.NewStream(context.Background(),
		&grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	out := new(wrapperspb.Int32Value)
	if err := cs.RecvMsg(out); err != context.Canceled {
		t.Errorf("terminal error = %v, want the interceptor's rejection", err)
	}
	if handlerRan {
		t.Errorf("the handler ran even though the interceptor refused the call")
	}
}

// An empty / absent chain installs nothing — a tier with no stream
// interceptors keeps dispatching straight to its handler.
func TestConn_WithStreamInterceptors_Empty(t *testing.T) {
	conn := inprocgrpc.New(inprocgrpc.WithStreamInterceptors())
	conn.RegisterService(countServiceDesc(), &countServer{})
	cs, err := conn.NewStream(context.Background(),
		&grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	got, err := drain(t, cs, 2)
	if err == nil || len(got) != 2 {
		t.Errorf("got %v / %v, want 2 messages then a terminal error", got, err)
	}
}
