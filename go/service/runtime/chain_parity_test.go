package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/service/inprocgrpc"
)

// A tier's interceptor chain behaves the same whether the tier is a
// separate process (GRPCComponent behind a network hop) or composed into
// one binary (the in-process bridge): the interceptors run in the same
// order around the handler, they see the caller's metadata, and a panic
// inside the chain is contained and reaches the caller as Internal.
func TestTierChain_RunsTheSameComposedAndSplit(t *testing.T) {
	type outcome struct {
		order string
		code  codes.Code
	}
	// run calls the ping method twice through invoke: once normally and
	// once with the metadata that makes the inner interceptor panic.
	run := func(t *testing.T, serve func(chain []grpc.UnaryServerInterceptor) grpc.ClientConnInterface) []outcome {
		var mu sync.Mutex
		var order []string
		mark := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
		mk := func(name string, panics bool) grpc.UnaryServerInterceptor {
			return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				mark(name + ":before:" + info.FullMethod)
				md, _ := metadata.FromIncomingContext(ctx)
				if panics && len(md.Get("x-boom")) > 0 {
					panic("interceptor panic")
				}
				resp, err := h(ctx, req)
				mark(name + ":after")
				return resp, err
			}
		}
		conn := serve([]grpc.UnaryServerInterceptor{mk("outer", false), mk("inner", true)})
		var out []outcome
		for _, ctx := range []context.Context{
			context.Background(),
			metadata.AppendToOutgoingContext(context.Background(), "x-boom", "1"),
		} {
			mu.Lock()
			order = nil
			mu.Unlock()
			err := conn.Invoke(ctx, "/test.Ping/Ping", &emptypb.Empty{}, new(emptypb.Empty))
			mu.Lock()
			out = append(out, outcome{strings.Join(order, ","), status.Code(err)})
			mu.Unlock()
		}
		return out
	}

	split := run(t, func(chain []grpc.UnaryServerInterceptor) grpc.ClientConnInterface {
		noTLS(t)
		addr := freeAddr(t)
		stop := runComponent(t, Config{
			Addr:              addr,
			UnaryInterceptors: chain,
			RegisterServices:  func(s *grpc.Server) error { s.RegisterService(&pingDesc, struct{}{}); return nil },
		})
		t.Cleanup(func() { _ = stop() })
		return plainConn(t, addr)
	})
	composed := run(t, func(chain []grpc.UnaryServerInterceptor) grpc.ClientConnInterface {
		conn := inprocgrpc.New(inprocgrpc.WithUnaryInterceptors(chain...))
		conn.RegisterService(&pingDesc, struct{}{})
		return conn
	})

	want := []outcome{
		{"outer:before:/test.Ping/Ping,inner:before:/test.Ping/Ping,inner:after,outer:after", codes.OK},
		{"outer:before:/test.Ping/Ping,inner:before:/test.Ping/Ping", codes.Internal},
	}
	for i := range want {
		if split[i] != want[i] {
			t.Errorf("split call %d = %+v, want %+v", i, split[i], want[i])
		}
		if composed[i] != want[i] {
			t.Errorf("composed call %d = %+v, want %+v", i, composed[i], want[i])
		}
	}
}
