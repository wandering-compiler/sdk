package inprocgrpc_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// TestConn_Invoke_HonoursHeaderAndTrailerCallOptions pins C-F4. Invoke used
// to take `_ ...grpc.CallOption` and drop every option on the floor, so
// grpc.Header(&md) came back nil no matter what the handler set — silently,
// with no error anywhere.
//
// It is exercised by shipped wiring: the composed -server routes the distx
// coordinator over the storage Conn, and distx's client reads the proxy
// routing token (`w17-conn-id`) with exactly this call option. Empty is
// coincidentally correct today (no in-process proxy mints one), which is
// what makes it a silent divergence rather than a visible bug — the same
// class of composed-vs-standalone disagreement that has bitten before.
func TestConn_Invoke_HonoursHeaderAndTrailerCallOptions(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(unaryDesc("test.Record", "Record", func(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		if err := grpc.SetHeader(ctx, metadata.Pairs("w17-conn-id", "conn-42")); err != nil {
			return nil, err
		}
		if err := grpc.SetTrailer(ctx, metadata.Pairs("w17-rows-affected", "3")); err != nil {
			return nil, err
		}
		return wrapperspb.String("ok"), nil
	}), nil)

	var header, trailer metadata.MD
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), recordMethod, wrapperspb.String("x"), out,
		grpc.Header(&header), grpc.Trailer(&trailer)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if v := header.Get("w17-conn-id"); len(v) != 1 || v[0] != "conn-42" {
		t.Errorf("grpc.Header() got %v, want the handler's w17-conn-id: a CallOption-dependent client "+
			"feature fails silently in-process while working over the wire", header)
	}
	if v := trailer.Get("w17-rows-affected"); len(v) != 1 || v[0] != "3" {
		t.Errorf("grpc.Trailer() got %v, want the handler's trailer", trailer)
	}
}

// A handler that sets no header must leave the caller's MD empty rather
// than crash the bridge — and an unknown CallOption stays inert (there is
// no wire for it to configure), which is the documented contract.
func TestConn_Invoke_NoHeaderSet_IsEmptyNotBroken(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(echoServiceDesc(), &echoServer{})

	var header metadata.MD
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(context.Background(), echoMethod, wrapperspb.String("x"), out,
		grpc.Header(&header), grpc.WaitForReady(true)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(header) != 0 {
		t.Errorf("header = %v, want empty", header)
	}
	if out.GetValue() != "echo:x" {
		t.Errorf("response = %q", out.GetValue())
	}
}

// TestConn_Stream_HeaderAndTrailer is the streaming half: ClientStream's
// Header()/Trailer() returned (nil, nil) unconditionally, so a caller
// waiting on stream headers got nothing. Header() must also UNBLOCK when
// the handler ends without sending any (the wire delivers trailers-only
// there); a Header() that could hang forever is worse than one that lies.
func TestConn_Stream_HeaderAndTrailer(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName: "Count", ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				if err := stream.SetHeader(metadata.Pairs("w17-conn-id", "conn-42")); err != nil {
					return err
				}
				if err := stream.SendHeader(nil); err != nil {
					return err
				}
				stream.SetTrailer(metadata.Pairs("w17-rows-affected", "1"))
				return stream.SendMsg(wrapperspb.Int32(7))
			},
		}},
	}, nil)

	cs, err := conn.NewStream(context.Background(), &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	done := make(chan metadata.MD, 1)
	go func() {
		md, herr := cs.Header()
		if herr != nil {
			t.Errorf("Header(): %v", herr)
		}
		done <- md
	}()

	var header metadata.MD
	select {
	case header = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ClientStream.Header() blocked forever — the handler had already sent its headers")
	}
	if v := header.Get("w17-conn-id"); len(v) != 1 || v[0] != "conn-42" {
		t.Errorf("stream Header() got %v, want the handler's w17-conn-id", header)
	}

	// Drain to completion; the trailer is readable once the stream ends.
	for {
		msg := new(wrapperspb.Int32Value)
		if err := cs.RecvMsg(msg); err != nil {
			break
		}
	}
	if v := cs.Trailer().Get("w17-rows-affected"); len(v) != 1 || v[0] != "1" {
		t.Errorf("stream Trailer() got %v, want the handler's trailer", cs.Trailer())
	}
}

// A handler that sends nothing at all must still release a caller parked in
// Header() — the stream ending IS the answer.
func TestConn_Stream_HeaderUnblocksWhenHandlerEndsSilently(t *testing.T) {
	conn := inprocgrpc.New()
	conn.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Counter",
		Streams: []grpc.StreamDesc{{
			StreamName: "Count", ServerStreams: true,
			Handler: func(any, grpc.ServerStream) error { return nil },
		}},
	}, nil)

	cs, err := conn.NewStream(context.Background(), &grpc.StreamDesc{StreamName: "Count", ServerStreams: true}, countMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, herr := cs.Header(); herr != nil {
			t.Errorf("Header(): %v", herr)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ClientStream.Header() never returned after the handler finished")
	}
}
