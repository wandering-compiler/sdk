package grpcx

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
)

// A streaming call opened through DialOpts relays the principal stamps to
// the downstream tier, like a unary one, and nothing else.
func TestDialOpts_StreamRelaysThePrincipalOnly(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan metadata.MD, 1)
	srv := grpc.NewServer()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Watch",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Watch",
			ServerStreams: true,
			Handler: func(_ any, ss grpc.ServerStream) error {
				md, _ := metadata.FromIncomingContext(ss.Context())
				seen <- md
				return nil
			},
		}},
	}, struct{}{})
	go func() { _ = srv.Serve(l) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(l.Addr().String(), DialOpts(false)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-w17-scope-org_id", "org-7", "x-w17-paging-limit", "10"))
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Watch", ServerStreams: true}, "/test.Watch/Watch")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SendMsg(&emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	_ = cs.CloseSend()
	select {
	case md := <-seen:
		if got := md.Get("x-w17-scope-org_id"); len(got) != 1 || got[0] != "org-7" {
			t.Errorf("principal not relayed on a stream: %v", got)
		}
		if got := md.Get("x-w17-paging-limit"); len(got) != 0 {
			t.Errorf("non-principal metadata relayed on a stream: %v", got)
		}
	case <-ctx.Done():
		t.Fatal("the stream never reached the server")
	}
}
