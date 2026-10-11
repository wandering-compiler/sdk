package server

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"
)

// startRun runs Run in the background and returns the address it listens
// on (via the listen seam) and its result channel.
func startRun(t *testing.T, ctx context.Context, cfg Config) (net.Listener, <-chan error) {
	t.Helper()
	lisCh := make(chan net.Listener, 1)
	orig := listen
	listen = func(network, addr string) (net.Listener, error) {
		l, err := orig(network, addr)
		if err == nil {
			lisCh <- l
		}
		return l, err
	}
	t.Cleanup(func() { listen = orig })
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	select {
	case l := <-lisCh:
		return l, done
	case err := <-done:
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not listen")
	}
	return nil, nil
}

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Once shutdown begins, a client watching health sees NOT_SERVING while the
// drain is still running, instead of SERVING until the connection drops.
func TestRun_HealthReportsNotServingOnceDrainStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, done := startRun(t, ctx, Config{
		Addr:             "127.0.0.1:0",
		HealthServer:     DefaultHealthServer(),
		ShutdownTimeout:  3 * time.Second, // the open Watch holds the drain
		RegisterServices: func(*grpc.Server) error { return nil },
	})

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	w, err := grpc_health_v1.NewHealthClient(dial(t, l.Addr().String())).Watch(watchCtx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Recv()
	if err != nil || first.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("first health update = %v / %v, want SERVING", first, err)
	}

	start := time.Now()
	cancel()
	next, err := w.Recv()
	if err != nil {
		t.Fatalf("health stream ended (%v) without reporting NOT_SERVING", err)
	}
	if next.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Errorf("health during drain = %v, want NOT_SERVING", next.Status)
	}
	if time.Since(start) > time.Second {
		t.Errorf("NOT_SERVING arrived after %v — only at the hard stop, not when the drain began", time.Since(start))
	}
	watchCancel() // release the drain
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// holdDesc is a server-streaming service whose handler blocks until its
// context ends, recording that it ended.
func holdDesc(started, ended chan struct{}) *grpc.ServiceDesc {
	var once sync.Once
	return &grpc.ServiceDesc{
		ServiceName: "test.Hold",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Hold",
			ServerStreams: true,
			Handler: func(_ any, ss grpc.ServerStream) error {
				once.Do(func() { close(started) })
				<-ss.Context().Done()
				close(ended)
				return nil
			},
		}},
	}
}

// When Serve fails on its own, the server is stopped before OnShutdown runs,
// so a handler on an already-accepted connection is not still running while
// the hooks close the resources it uses. Run returns the serve error.
func TestRun_ServeErrorStopsServerBeforeOnShutdown(t *testing.T) {
	started, ended := make(chan struct{}), make(chan struct{})
	handlerEndedFirst := make(chan bool, 1)
	l, done := startRun(t, context.Background(), Config{
		Addr: "127.0.0.1:0",
		RegisterServices: func(s *grpc.Server) error {
			s.RegisterService(holdDesc(started, ended), struct{}{})
			return nil
		},
		OnShutdown: func() {
			select {
			case <-ended:
				handlerEndedFirst <- true
			case <-time.After(time.Second):
				handlerEndedFirst <- false
			}
		},
	})

	desc := &grpc.StreamDesc{StreamName: "Hold", ServerStreams: true}
	cs, err := dial(t, l.Addr().String()).NewStream(context.Background(), desc, "/test.Hold/Hold")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SendMsg(&emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	_ = cs.CloseSend()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}

	_ = l.Close() // Serve fails on its own; ctx is never cancelled

	select {
	case err := <-done:
		if err == nil {
			t.Error("Run = nil, want the serve error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the listener failed")
	}
	if !<-handlerEndedFirst {
		t.Error("OnShutdown ran while a handler was still serving on an accepted connection")
	}
}

// grpc-go panics when given two singular interceptor options; Run returns
// that as a configuration error instead of crashing the process.
func TestRun_ConflictingInterceptorOptionsAreAnError(t *testing.T) {
	noop := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(ctx, req)
	}
	err := Run(context.Background(), Config{
		Addr:             "127.0.0.1:0",
		ServerOptions:    []grpc.ServerOption{grpc.UnaryInterceptor(noop), grpc.UnaryInterceptor(noop)},
		RegisterServices: func(*grpc.Server) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ChainUnaryInterceptor") {
		t.Fatalf("Run = %v, want an error pointing at the chained interceptor option", err)
	}
}
