package observx

import (
	"context"
	"errors"
	"testing"
)

func acquireForTest(t *testing.T, c Config) func(context.Context) error {
	t.Helper()
	release, err := Acquire(c)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", c.ServiceName, err)
	}
	t.Cleanup(func() { _ = release(context.Background()) })
	return release
}

func liveMeterProvider() bool {
	mu.Lock()
	defer mu.Unlock()
	return meterProvider != nil
}

// INVARIANT: holds with the same Config share one setup; it stays live
// until the LAST hold is released, and only that release shuts it down.
func TestAcquire_SameConfigSharesUntilLastRelease(t *testing.T) {
	resetForTest(t)
	c := Config{ServiceName: "svc", EnableMetrics: true}
	first := acquireForTest(t, c)
	mu.Lock()
	installed := meterProvider
	mu.Unlock()

	second := acquireForTest(t, c)
	mu.Lock()
	same := meterProvider == installed
	mu.Unlock()
	if !same {
		t.Fatal("second Acquire with the same Config replaced the meter provider under the first")
	}

	if err := first(context.Background()); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if !liveMeterProvider() {
		t.Fatal("releasing one of two holds shut observability down under the other")
	}
	if err := first(context.Background()); err != nil {
		t.Fatalf("second call of the same release: %v, want a no-op", err)
	}
	if !liveMeterProvider() {
		t.Fatal("calling a release twice dropped a second hold")
	}
	if err := second(context.Background()); err != nil {
		t.Fatalf("last release: %v", err)
	}
	if liveMeterProvider() {
		t.Fatal("last release left the meter provider installed")
	}
}

// INVARIANT: while held, a different Config, a direct MustInit and a
// direct Shutdown are refused with ErrHeld and change nothing.
func TestAcquire_HeldStateIsNotReplaced(t *testing.T) {
	resetForTest(t)
	acquireForTest(t, Config{ServiceName: "first", EnableMetrics: true})
	mu.Lock()
	installed := meterProvider
	mu.Unlock()

	if _, err := Acquire(Config{ServiceName: "second", EnableMetrics: true}); !errors.Is(err, ErrHeld) {
		t.Errorf("Acquire with a different Config = %v, want ErrHeld", err)
	}
	if err := MustInit(Config{ServiceName: "second"}); !errors.Is(err, ErrHeld) {
		t.Errorf("MustInit while held = %v, want ErrHeld", err)
	}
	if err := Shutdown(context.Background()); !errors.Is(err, ErrHeld) {
		t.Errorf("Shutdown while held = %v, want ErrHeld", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if meterProvider != installed {
		t.Error("the held meter provider was replaced or shut down")
	}
	if cfg.ServiceName != "first" {
		t.Errorf("service name = %q, want the held %q", cfg.ServiceName, "first")
	}
}

// A failed first Acquire leaves no hold behind, so a corrected retry and
// a plain MustInit still work.
func TestAcquire_FailedInitHoldsNothing(t *testing.T) {
	resetForTest(t)
	if _, err := Acquire(Config{}); err == nil {
		t.Fatal("Acquire with no service name succeeded")
	}
	if err := MustInit(Config{ServiceName: "after"}); err != nil {
		t.Fatalf("MustInit after a failed Acquire: %v", err)
	}
	if err := Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
