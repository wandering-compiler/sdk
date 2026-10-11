package bootstrap_test

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
)

// Two supervisors in one process (a test binary, a composed binary): the
// second must not swap observability under the first's running
// components. With the same Config they share one setup, and the one that
// finishes first does not shut it down under the other; with a different
// Config the second is refused.
func TestRun_SecondSupervisorDoesNotSwapObservability(t *testing.T) {
	obs := observx.Config{ServiceName: "bootstrap-shared", EnableMetrics: true}
	ctxA, cancelA := context.WithCancel(context.Background())
	started := make(chan struct{})
	doneA := make(chan error, 1)
	go func() {
		doneA <- bootstrap.Run(ctxA, obs, bootstrap.Func(func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		}))
	}()
	<-started
	provider := otel.GetMeterProvider()

	// Same Config: runs, and its end leaves A's telemetry working.
	if err := bootstrap.Run(context.Background(), obs, bootstrap.Func(func(context.Context) error { return nil })); err != nil {
		t.Fatalf("second supervisor with the same Config: %v", err)
	}
	if got := otel.GetMeterProvider(); got != provider {
		t.Error("a second supervisor replaced the global meter provider under the first")
	}

	// Different Config: refused before any component starts.
	ran := false
	err := bootstrap.Run(context.Background(), observx.Config{ServiceName: "bootstrap-other"},
		bootstrap.Func(func(context.Context) error { ran = true; return nil }))
	if err == nil || !strings.Contains(err.Error(), "held by a running supervisor") {
		t.Errorf("second supervisor with a different Config = %v, want a refusal", err)
	}
	if ran {
		t.Error("a refused supervisor started its components")
	}

	cancelA()
	if err := <-doneA; err != nil {
		t.Fatalf("first supervisor: %v", err)
	}
}
