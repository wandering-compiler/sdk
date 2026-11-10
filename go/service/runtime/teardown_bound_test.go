package runtime

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
)

// The component's own teardown (the wait for after-commit work and the
// Shutdowns hooks, 30 s each) is longer than the supervisor's shutdown
// deadline (20 s by default). Without a common bound a slow hook ran until
// the supervisor gave up and the process exited under it, with the hook's
// end never reported. The hooks' context now ends before the supervisor's
// deadline, so a hook that honours it returns and the shutdown completes
// inside the bound.
func TestGRPCComponent_ShutdownHooksEndBeforeTheSupervisorsDeadline(t *testing.T) {
	noTLS(t)
	t.Setenv(bootstrap.EnvShutdownDeadline, "3")
	hookEnded := make(chan error, 1)
	cfg := Config{
		Addr:             freeAddr(t),
		RegisterServices: func(*grpc.Server) error { return nil },
		Shutdowns: []func(context.Context) error{func(ctx context.Context) error {
			<-ctx.Done() // a hook that would take as long as it is allowed
			hookEnded <- ctx.Err()
			return ctx.Err()
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(ctx, observx.Config{ServiceName: "teardown-bound"}, GRPCComponent(cfg)) }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want a shutdown that completed inside the supervisor's deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("shutdown took %s, past the 3s deadline", took)
	}
	select {
	case err := <-hookEnded:
		if err == nil {
			t.Error("the hook's context ended without an error")
		}
	default:
		t.Error("the hook was still running when Run returned")
	}
}
