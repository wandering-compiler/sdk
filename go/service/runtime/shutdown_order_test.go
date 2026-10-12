package runtime

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// holdDesc is a one-method service whose handler runs fn, through the
// server's interceptor chain like a generated handler.
func holdDesc(fn func(ctx context.Context)) *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "test.Hold",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Hold",
			Handler: func(srv any, ctx context.Context, dec func(any) error, icpt grpc.UnaryServerInterceptor) (any, error) {
				in := new(emptypb.Empty)
				if err := dec(in); err != nil {
					return nil, err
				}
				h := func(ctx context.Context, _ any) (any, error) { fn(ctx); return &emptypb.Empty{}, nil }
				if icpt == nil {
					return h(ctx, in)
				}
				return icpt(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Hold/Hold"}, h)
			},
		}},
	}
}

type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(e string) { l.mu.Lock(); l.ev = append(l.ev, e); l.mu.Unlock() }
func (l *eventLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

// After a forced stop, a handler still finishing its request keeps the
// resources it uses: the shutdown hooks that close them run only once it
// has returned.
func TestGRPCComponent_ShutdownHooksWaitForHandlersAfterForcedStop(t *testing.T) {
	noTLS(t)
	var log eventLog
	entered := make(chan struct{})
	var once sync.Once
	addr := freeAddr(t)
	stop := runComponent(t, Config{
		Addr:            addr,
		ShutdownTimeout: 100 * time.Millisecond,
		RegisterServices: func(s *grpc.Server) error {
			s.RegisterService(holdDesc(func(ctx context.Context) {
				once.Do(func() { close(entered) })
				<-ctx.Done()                       // the forced stop cancels it
				time.Sleep(300 * time.Millisecond) // still using the resource
				log.add("handler returned")
			}), struct{}{})
			return nil
		},
		Shutdowns: []func(context.Context) error{func(context.Context) error {
			log.add("resource closed")
			return nil
		}},
	})
	conn := plainConn(t, addr)
	go func() { _ = conn.Invoke(context.Background(), "/test.Hold/Hold", &emptypb.Empty{}, &emptypb.Empty{}) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	if err := stop(); err != nil {
		t.Fatalf("stop = %v", err)
	}
	if got, want := log.get(), []string{"handler returned", "resource closed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v: a resource was closed while a request still used it", got, want)
	}
}
