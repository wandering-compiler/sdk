package inprocgrpc_test

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// keepServer answers with a message it keeps, and keeps the request it got,
// so the test can mutate both after the call returned.
type keepServer struct {
	gotReq  *wrapperspb.BytesValue
	sentRes *wrapperspb.BytesValue
}

func keepServiceDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "test.Keep",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Keep",
			Handler: func(srv any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.BytesValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				s := srv.(*keepServer)
				s.gotReq = in
				s.sentRes = &wrapperspb.BytesValue{Value: append([]byte("echo:"), in.GetValue()...)}
				return s.sentRes, nil
			},
		}},
	}
}

// Over the wire each side has its own bytes. In process the bridge must
// deep-copy both ways: a caller reusing its request buffer after the call,
// or a handler mutating the message it returned (a cached value it keeps
// updating), must not change what the other side holds.
func TestConn_MessagesAreDeepCopiedBothWays(t *testing.T) {
	conn := inprocgrpc.New()
	srv := &keepServer{}
	conn.RegisterService(keepServiceDesc(), srv)

	args := &wrapperspb.BytesValue{Value: []byte("abc")}
	reply := new(wrapperspb.BytesValue)
	if err := conn.Invoke(context.Background(), "/test.Keep/Keep", args, reply); err != nil {
		t.Fatal(err)
	}
	args.Value[0] = 'X'
	srv.sentRes.Value[0] = 'Y'
	if !bytes.Equal(srv.gotReq.GetValue(), []byte("abc")) {
		t.Errorf("the handler's request changed with the caller's buffer: %q", srv.gotReq.GetValue())
	}
	if !bytes.Equal(reply.GetValue(), []byte("echo:abc")) {
		t.Errorf("the caller's reply changed with the handler's message: %q", reply.GetValue())
	}
}
