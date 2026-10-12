package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/service/bootstrap"
)

// A handler that does not return even after the forced stop and a grace is
// abandoned: the component still returns in bounded time, and neither its
// own shutdown hooks nor the supervisor's resource closers run under it.
func TestGRPCComponent_AbandonedHandlerHoldsResources(t *testing.T) {
	noTLS(t)
	saved := handlerReturnGrace
	handlerReturnGrace = 200 * time.Millisecond
	t.Cleanup(func() { handlerReturnGrace = saved })

	var log eventLog
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var once sync.Once
	addr := freeAddr(t)
	cfg := Config{
		ServiceName:     "abandon-test",
		Addr:            addr,
		ShutdownTimeout: 100 * time.Millisecond,
		RegisterServices: func(s *grpc.Server) error {
			s.RegisterService(holdDesc(func(ctx context.Context) {
				once.Do(func() { close(entered) })
				<-ctx.Done()
				<-release // ignores its context past any grace
			}), struct{}{})
			return nil
		},
		Shutdowns: []func(context.Context) error{func(context.Context) error {
			log.add("hook")
			return nil
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bootstrap.RunGraceful(ctx, cfg.ObservxConfig(), []func(context.Context) error{
			func(context.Context) error { log.add("resource"); return nil },
		}, GRPCComponent(cfg))
	}()
	conn := plainConn(t, addr)
	go func() { _ = conn.Invoke(context.Background(), "/test.Hold/Hold", &emptypb.Empty{}, &emptypb.Empty{}) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunGraceful = %v, want nil: the transport did stop when asked", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown was not bounded with a handler abandoned")
	}
	if got := log.get(); len(got) != 0 {
		t.Fatalf("closed under a running handler: %v", got)
	}
}
