package server

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

func TestDefaultHealthServer(t *testing.T) {
	hs := DefaultHealthServer()
	resp, err := hs.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("root status = %v, want SERVING", resp.Status)
	}
}

func TestRun_ValidationErrors(t *testing.T) {
	if err := Run(context.Background(), Config{RegisterServices: func(*grpc.Server) error { return nil }}); err == nil {
		t.Error("want error for missing Addr")
	}
	if err := Run(context.Background(), Config{Addr: "127.0.0.1:0"}); err == nil {
		t.Error("want error for missing RegisterServices")
	}
}

func TestRun_ListenError(t *testing.T) {
	// Occupy a port, then ask Run to bind the same addr → listen fails.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := Config{Addr: l.Addr().String(), RegisterServices: func(*grpc.Server) error { return nil }}
	if err := Run(context.Background(), cfg); err == nil {
		t.Error("want listen error for an already-bound address")
	}
}

func TestRun_RegisterError(t *testing.T) {
	wantErr := context.Canceled // any sentinel
	cfg := Config{
		Addr:             "127.0.0.1:0",
		RegisterServices: func(*grpc.Server) error { return wantErr },
	}
	err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("want error when RegisterServices fails")
	}
}

func TestRun_GracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	shutdownRan := make(chan struct{})
	done := make(chan error, 1)

	cfg := Config{
		Addr:             "127.0.0.1:0",
		Reflection:       true,
		HealthServer:     DefaultHealthServer(),
		RegisterServices: func(*grpc.Server) error { return nil },
		OnShutdown:       func() { close(shutdownRan) },
	}
	go func() { done <- Run(ctx, cfg) }()

	// Give the server a moment to come up, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	select {
	case <-shutdownRan:
	default:
		t.Error("OnShutdown was not called")
	}
}

// TestGracefulStop_TimeoutHardStop drives the timeout arm of gracefulStop: an
// in-flight server-streaming RPC keeps GracefulStop blocked past the timeout,
// forcing the hard Stop().
func TestGracefulStop_TimeoutHardStop(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, hs)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Open a long-lived Watch stream → an in-flight RPC that GracefulStop waits on.
	cli := grpc_health_v1.NewHealthClient(conn)
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	if _, err := cli.Watch(streamCtx, &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	// Let the stream register server-side.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	gracefulStop(srv, 150*time.Millisecond) // GracefulStop blocks → Stop() at timeout
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("gracefulStop returned too fast (%v) — timeout arm not exercised", elapsed)
	}
}

// TestGracefulStop_ZeroBlocks covers the timeout==0 branch (plain GracefulStop,
// no in-flight RPCs so it returns immediately).
func TestGracefulStop_ZeroBlocks(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(lis) }()
	time.Sleep(50 * time.Millisecond)
	gracefulStop(srv, 0)
}
