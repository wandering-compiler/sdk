package inprocgrpc

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// protomsg aliases the channel element type used by inprocStream.
type protomsg = proto.Message

// --- stream view stub accessors (all no-ops in-process) ---

func TestStreamStubAccessors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &inprocStream{clientCtx: ctx, serverCtx: ctx}

	cs := clientStream{p: p}
	if cs.Context() != ctx {
		t.Error("clientStream.Context mismatch")
	}
	if md, err := cs.Header(); md != nil || err != nil {
		t.Errorf("clientStream.Header = %v,%v; want nil,nil", md, err)
	}
	if cs.Trailer() != nil {
		t.Error("clientStream.Trailer should be nil")
	}

	ss := serverStream{p: p}
	if ss.Context() != ctx {
		t.Error("serverStream.Context mismatch")
	}
	if err := ss.SetHeader(nil); err != nil {
		t.Errorf("SetHeader = %v", err)
	}
	if err := ss.SendHeader(nil); err != nil {
		t.Errorf("SendHeader = %v", err)
	}
	ss.SetTrailer(nil) // no-op, must not panic
}

// --- cloneMsg / copyProto error arms ---

func TestCloneMsg_NonProto(t *testing.T) {
	if _, err := cloneMsg("not a proto"); err == nil {
		t.Fatal("want error cloning a non-proto value")
	}
}

func TestCopyProto_Errors(t *testing.T) {
	if err := copyProto("dst-not-proto", wrapperspb.String("x")); err == nil {
		t.Error("want error when dst is not a proto")
	}
	if err := copyProto(wrapperspb.String("x"), "src-not-proto"); err == nil {
		t.Error("want error when src is not a proto")
	}
}

// --- SendMsg / RecvMsg error + cancel arms ---

func TestClientStream_SendMsg_CloneError(t *testing.T) {
	p := &inprocStream{clientCtx: context.Background()}
	if err := (clientStream{p: p}).SendMsg("not proto"); err == nil {
		t.Fatal("want clone error")
	}
}

func TestServerStream_SendMsg_CloneError(t *testing.T) {
	p := &inprocStream{serverCtx: context.Background()}
	if err := (serverStream{p: p}).SendMsg("not proto"); err == nil {
		t.Fatal("want clone error")
	}
}

func TestClientStream_SendMsg_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &inprocStream{clientCtx: ctx, c2s: make(chan protomsg)} // unbuffered, no reader
	if err := (clientStream{p: p}).SendMsg(wrapperspb.String("x")); err == nil {
		t.Fatal("want ctx error on a cancelled client stream")
	}
}

func TestServerStream_SendMsg_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &inprocStream{serverCtx: ctx, s2c: make(chan protomsg)}
	if err := (serverStream{p: p}).SendMsg(wrapperspb.String("x")); err == nil {
		t.Fatal("want ctx error on a cancelled server stream")
	}
}

func TestClientStream_RecvMsg_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &inprocStream{clientCtx: ctx, s2c: make(chan protomsg)}
	if err := (clientStream{p: p}).RecvMsg(wrapperspb.String("")); err == nil {
		t.Fatal("want ctx error")
	}
}

func TestServerStream_RecvMsg_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &inprocStream{serverCtx: ctx, c2s: make(chan protomsg)}
	if err := (serverStream{p: p}).RecvMsg(wrapperspb.String("")); err == nil {
		t.Fatal("want ctx error")
	}
}

func TestClientStream_RecvMsg_EOFAndError(t *testing.T) {
	// Closed s2c with no error → io.EOF.
	p := &inprocStream{clientCtx: context.Background(), s2c: make(chan protomsg)}
	close(p.s2c)
	if err := (clientStream{p: p}).RecvMsg(wrapperspb.String("")); err != io.EOF {
		t.Errorf("RecvMsg on closed stream = %v, want io.EOF", err)
	}
	// Closed s2c WITH an error → that error.
	p2 := &inprocStream{clientCtx: context.Background(), s2c: make(chan protomsg), err: errors.New("boom")}
	close(p2.s2c)
	if err := (clientStream{p: p2}).RecvMsg(wrapperspb.String("")); err == nil || err == io.EOF {
		t.Errorf("RecvMsg on errored stream = %v, want the stored error", err)
	}
}

func TestServerStream_RecvMsg_EOFOnCloseSend(t *testing.T) {
	p := &inprocStream{serverCtx: context.Background(), c2s: make(chan protomsg)}
	close(p.c2s)
	if err := (serverStream{p: p}).RecvMsg(wrapperspb.String("")); err != io.EOF {
		t.Errorf("RecvMsg after CloseSend = %v, want io.EOF", err)
	}
}

// --- chainUnary single-interceptor arm ---

func TestChainUnary_Single(t *testing.T) {
	ran := false
	one := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		ran = true
		return h(ctx, req)
	}
	chained := chainUnary([]grpc.UnaryServerInterceptor{one})
	if chained == nil {
		t.Fatal("single interceptor should not chain to nil")
	}
	_, _ = chained(context.Background(), nil, &grpc.UnaryServerInfo{},
		func(ctx context.Context, req any) (any, error) { return nil, nil })
	if !ran {
		t.Error("the single interceptor did not run")
	}
}

// --- Invoke error arms ---

func TestInvoke_HandlerError(t *testing.T) {
	c := New()
	c.methods["/x/Y"] = &methodEntry{
		handler: func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
			return nil, errors.New("handler boom")
		},
	}
	err := c.Invoke(context.Background(), "/x/Y", wrapperspb.String("in"), wrapperspb.String(""))
	if err == nil {
		t.Fatal("want the handler error surfaced")
	}
}

func TestInvoke_CopyReplyError(t *testing.T) {
	c := New()
	c.methods["/x/Y"] = &methodEntry{
		handler: func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
			return "not-a-proto-response", nil // copyProto(reply, resp) fails
		},
	}
	err := c.Invoke(context.Background(), "/x/Y", wrapperspb.String("in"), wrapperspb.String(""))
	if err == nil {
		t.Fatal("want error when the response can't be copied into reply")
	}
}
