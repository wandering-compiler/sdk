package runtime

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func TestObservxConfig_Projection(t *testing.T) {
	cfg := Config{
		ServiceName:    "app-storage",
		ServiceVersion: "abc123",
		Environment:    "prod",
		SentryDSN:      "dsn",
		OTelEndpoint:   "otel:4317",
		SampleRate:     0.5,
		EnableMetrics:  true,
	}
	o := cfg.ObservxConfig()
	if o.ServiceName != "app-storage" || o.ServiceVersion != "abc123" || o.Environment != "prod" ||
		o.SentryDSN != "dsn" || o.OTelEndpoint != "otel:4317" || o.SampleRate != 0.5 || !o.EnableMetrics {
		t.Errorf("ObservxConfig projection mismatch: %+v", o)
	}
}

// TestGRPCComponent_ServesAndDrains runs the component to a clean ctx-cancel
// shutdown and asserts the drain hooks fire (incl. a nil hook being skipped).
func TestGRPCComponent_ServesAndDrains(t *testing.T) {
	drained := make(chan struct{})
	cfg := Config{
		Addr:             "127.0.0.1:0",
		Reflection:       true,
		RegisterServices: func(*grpc.Server) error { return nil },
		UnaryInterceptors: []grpc.UnaryServerInterceptor{
			func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				return h(ctx, req)
			},
		},
		Shutdowns: []func(context.Context) error{
			nil, // exercises the nil-skip
			func(context.Context) error { close(drained); return nil },
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- GRPCComponent(cfg).Run(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("component did not stop after ctx cancel")
	}
	select {
	case <-drained:
	default:
		t.Error("shutdown drain hook did not run")
	}
}

// TestGRPCComponent_NoShutdowns covers the empty-Shutdowns early-return arm.
func TestGRPCComponent_NoShutdowns(t *testing.T) {
	cfg := Config{
		Addr:             "127.0.0.1:0",
		RegisterServices: func(*grpc.Server) error { return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- GRPCComponent(cfg).Run(ctx) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("component did not stop")
	}
}

// TestServe_StandaloneLifecycle runs Serve under the supervisor and shuts it
// down via ctx cancel.
func TestServe_StandaloneLifecycle(t *testing.T) {
	cfg := Config{
		ServiceName:      "test-runtime",
		Addr:             "127.0.0.1:0",
		RegisterServices: func(*grpc.Server) error { return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}
