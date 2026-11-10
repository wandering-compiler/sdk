package runtime

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	"github.com/wandering-compiler/sdk/go/core/grpcx"
	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// failDesc is a service whose unary method and server-streaming method each
// count a run and answer what fail returns for that run (nil = success).
func failDesc(runs *atomic.Int32, fail func(run int32) error) *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "test.Fail",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Unary",
			Handler: func(srv any, ctx context.Context, dec func(any) error, icpt grpc.UnaryServerInterceptor) (any, error) {
				in := new(emptypb.Empty)
				if err := dec(in); err != nil {
					return nil, err
				}
				h := func(context.Context, any) (any, error) {
					if err := fail(runs.Add(1)); err != nil {
						return nil, err
					}
					return &emptypb.Empty{}, nil
				}
				if icpt == nil {
					return h(ctx, in)
				}
				return icpt(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Fail/Unary"}, h)
			},
		}},
		Streams: []grpc.StreamDesc{{
			StreamName:    "Stream",
			ServerStreams: true,
			Handler: func(_ any, ss grpc.ServerStream) error {
				in := new(emptypb.Empty)
				if err := ss.RecvMsg(in); err != nil {
					return err
				}
				if err := fail(runs.Add(1)); err != nil {
					return err
				}
				return ss.SendMsg(&emptypb.Empty{})
			},
		}},
	}
}

var streamDesc = &grpc.StreamDesc{StreamName: "Stream", ServerStreams: true}

// call runs one unary call or one server stream to its end over conn.
func call(conn *grpc.ClientConn, stream bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !stream {
		return conn.Invoke(ctx, "/test.Fail/Unary", &emptypb.Empty{}, &emptypb.Empty{}, grpc.WaitForReady(true))
	}
	cs, err := conn.NewStream(ctx, streamDesc, "/test.Fail/Stream", grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	if err := cs.SendMsg(&emptypb.Empty{}); err != nil {
		return err
	}
	if err := cs.CloseSend(); err != nil {
		return err
	}
	for {
		if err := cs.RecvMsg(new(emptypb.Empty)); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// A runtime-built server answers an UNAVAILABLE its handler returned so that
// neither w17 dialer (grpcclient.Dial, grpcx.DialOpts) runs the handler
// again — unary or streaming — while a handler's replay-safe UNAVAILABLE
// (the database was never reached) is still retried.
func TestGRPCComponent_AHandlersUnavailableIsNotReplayed(t *testing.T) {
	noTLS(t)
	var runs atomic.Int32
	var replaySafe atomic.Bool
	addr := freeAddr(t)
	stop := runComponent(t, Config{
		Addr: addr,
		RegisterServices: func(s *grpc.Server) error {
			s.RegisterService(failDesc(&runs, func(run int32) error {
				if run != 1 {
					return nil
				}
				err := status.Error(codes.Unavailable, "the upstream went away after the write")
				if replaySafe.Load() {
					return grpcerr.ReplaySafe(err)
				}
				return err
			}), struct{}{})
			return nil
		},
	})
	defer func() { _ = stop() }()

	viaGrpcclient, err := grpcclient.Dial(addr, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = viaGrpcclient.Close() })
	viaGrpcx, err := grpc.NewClient(addr, grpcx.DialOpts(false)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = viaGrpcx.Close() })

	for _, dial := range []struct {
		name string
		conn *grpc.ClientConn
	}{{"grpcclient", viaGrpcclient}, {"grpcx", viaGrpcx}} {
		for _, stream := range []bool{false, true} {
			runs.Store(0)
			replaySafe.Store(false)
			err := call(dial.conn, stream)
			if status.Code(err) != codes.Unavailable || runs.Load() != 1 {
				t.Errorf("%s stream=%v: caller saw %v after %d handler runs, want the handler's UNAVAILABLE after one", dial.name, stream, err, runs.Load())
			}
			runs.Store(0)
			replaySafe.Store(true)
			if err := call(dial.conn, stream); err != nil || runs.Load() != 2 {
				t.Errorf("%s stream=%v replay-safe: caller saw %v after %d handler runs, want success after a retry", dial.name, stream, err, runs.Load())
			}
		}
	}
}
