package grpcx

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// The verified device id rides every dialled inter-tier hop, unary and
// streaming, exactly like the principal envelope: a tier two hops from the
// gateway still reads principal.DeviceID.
func TestDialOpts_RelaysTheDeviceID(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan metadata.MD, 2)
	srv := grpc.NewServer()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Dev",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Get",
			Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in := new(emptypb.Empty)
				if err := dec(in); err != nil {
					return nil, err
				}
				md, _ := metadata.FromIncomingContext(ctx)
				seen <- md
				return &emptypb.Empty{}, nil
			},
		}},
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
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-w17-user", "env", principal.DeviceMetadataKey, "jkt-1"))

	check := func(kind string) {
		t.Helper()
		select {
		case md := <-seen:
			dev, ok := principal.DeviceID(metadata.NewIncomingContext(context.Background(), md))
			if !ok || dev != "jkt-1" {
				t.Errorf("%s hop: device = (%q, %v), want jkt-1", kind, dev, ok)
			}
		case <-ctx.Done():
			t.Fatalf("%s never reached the server", kind)
		}
	}

	if err := conn.Invoke(ctx, "/test.Dev/Get", &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	check("unary")

	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Watch", ServerStreams: true}, "/test.Dev/Watch")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SendMsg(&emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	_ = cs.CloseSend()
	check("stream")
}
