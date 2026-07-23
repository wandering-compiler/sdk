package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
)

// testObs is a minimal observx config — empty DSN/endpoint keep
// both exporters off, so MustInit is a cheap no-op wiring.
var testObs = observx.Config{ServiceName: "bootstrap-test"}

// All components stop when the parent context is cancelled, and Run
// returns nil on a clean ctx-driven stop.
func TestRun_CtxCancel_StopsAllComponents(t *testing.T) {
	var aRan, bRan atomic.Bool
	a := bootstrap.Func(func(ctx context.Context) error {
		aRan.Store(true)
		<-ctx.Done()
		return nil
	})
	b := bootstrap.Func(func(ctx context.Context) error {
		bRan.Store(true)
		<-ctx.Done()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(ctx, testObs, a, b) }()

	// Cancel after both have started; Run must return promptly.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on clean ctx stop", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if !aRan.Load() || !bRan.Load() {
		t.Fatalf("both components should have run: a=%v b=%v", aRan.Load(), bRan.Load())
	}
}

// One component failing cancels the shared ctx so the sibling drains
// too, and Run surfaces the failing component's error.
func TestRun_ComponentError_DrainsSiblingAndReturnsErr(t *testing.T) {
	boom := errors.New("boom")
	var siblingStopped atomic.Bool

	failing := bootstrap.Func(func(ctx context.Context) error {
		return boom // exits immediately
	})
	sibling := bootstrap.Func(func(ctx context.Context) error {
		<-ctx.Done() // must be cancelled by the supervisor when failing exits
		siblingStopped.Store(true)
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(context.Background(), testObs, failing, sibling) }()

	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("Run returned %v, want %v", err, boom)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after a component failed")
	}
	if !siblingStopped.Load() {
		t.Fatal("sibling component was not drained when the failing one exited")
	}
}

// A panic in one component must NOT crash the process: the
// supervisor recovers it into a terminal error, cancels the shared
// context so siblings drain, and Run returns the panic-as-error.
func TestRun_ComponentPanic_RecoversDrainsSiblingAndReturnsErr(t *testing.T) {
	var siblingStopped atomic.Bool

	panicking := bootstrap.Func(func(ctx context.Context) error {
		panic("component blew up")
	})
	sibling := bootstrap.Func(func(ctx context.Context) error {
		<-ctx.Done() // must be cancelled when the panicking component is recovered
		siblingStopped.Store(true)
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(context.Background(), testObs, panicking, sibling) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil after a component panicked, want a recovered error")
		}
		if !strings.Contains(err.Error(), "component panic") ||
			!strings.Contains(err.Error(), "component blew up") {
			t.Fatalf("Run error %q does not carry the panic value", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after a component panicked (process likely crashed)")
	}
	if !siblingStopped.Load() {
		t.Fatal("sibling component was not drained when the panicking one was recovered")
	}
}

// Zero components: Run inits observability and returns immediately.
func TestRun_NoComponents_ReturnsNil(t *testing.T) {
	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(context.Background(), testObs) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run with no components = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with zero components")
	}
}

// RunGraceful closes shared resources ONLY after every component has
// returned (the "drain transport → then close resources" ordering), and
// in the order given. A resource that closes while a component is still
// draining is exactly the shutdown race RunGraceful exists to prevent.
func TestRunGraceful_ResourcesCloseAfterComponentsDrain(t *testing.T) {
	var componentReturned atomic.Bool
	var order []string
	var mu sync.Mutex

	// A transport that keeps draining briefly after ctx cancel.
	transport := bootstrap.Func(func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond) // simulate in-flight drain
		componentReturned.Store(true)
		return nil
	})

	resA := func(context.Context) error {
		if !componentReturned.Load() {
			t.Error("resource closed while a component was still draining")
		}
		mu.Lock()
		order = append(order, "A")
		mu.Unlock()
		return nil
	}
	resB := func(context.Context) error {
		mu.Lock()
		order = append(order, "B")
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bootstrap.RunGraceful(ctx, testObs,
			[]func(context.Context) error{resA, resB}, transport)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunGraceful = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGraceful did not return after ctx cancel")
	}
	if !componentReturned.Load() {
		t.Fatal("component did not run/return")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "A" || order[1] != "B" {
		t.Fatalf("resources closed in wrong order: got %v, want [A B]", order)
	}
}

// A nil resource closer is skipped (mirrors the Shutdowns contract).
func TestRunGraceful_NilResourceSkipped(t *testing.T) {
	comp := bootstrap.Func(func(ctx context.Context) error { <-ctx.Done(); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var ran atomic.Bool
	go func() {
		done <- bootstrap.RunGraceful(ctx, testObs,
			[]func(context.Context) error{nil, func(context.Context) error { ran.Store(true); return nil }}, comp)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunGraceful = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGraceful did not return")
	}
	if !ran.Load() {
		t.Fatal("non-nil resource closer should have run")
	}
}
