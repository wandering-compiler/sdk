package bootstrap_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
)

// A component that returns nil before anything asked it to stop still ends
// the run: its siblings are cancelled rather than left serving alone.
func TestRun_CleanEarlyReturnCancelsSiblings(t *testing.T) {
	sibling := bootstrap.Func(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	early := bootstrap.Func(func(context.Context) error { return nil })

	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(context.Background(), testObs, sibling, early) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a component returning nil early left its sibling running")
	}
}

// Observability stays held while components drain and is released once
// they have finished — also when a component failed at boot. A supervisor
// with a different Config is refused while it is held, so it probes both.
func TestRun_ObservabilityReleasedAfterComponentsFinish(t *testing.T) {
	obs := observx.Config{ServiceName: "bootstrap-flush-order"}
	other := observx.Config{ServiceName: "bootstrap-flush-other"}
	noop := bootstrap.Func(func(context.Context) error { return nil })

	var duringDrain error
	failing := bootstrap.Func(func(ctx context.Context) error { return errors.New("bind: address in use") })
	draining := bootstrap.Func(func(ctx context.Context) error {
		<-ctx.Done()
		duringDrain = bootstrap.Run(context.Background(), other, noop)
		return nil
	})
	if err := bootstrap.Run(context.Background(), obs, failing, draining); err == nil || !strings.Contains(err.Error(), "address in use") {
		t.Fatalf("Run = %v, want the boot failure", err)
	}
	if duringDrain == nil || !strings.Contains(duringDrain.Error(), "held by a running supervisor") {
		t.Errorf("a different Config during the drain = %v, want a refusal (observability still held)", duringDrain)
	}
	if err := bootstrap.Run(context.Background(), other, noop); err != nil {
		t.Errorf("a different Config after a failed run = %v, want it accepted (observability released)", err)
	}
}

// Once RunGraceful has returned, the supervisor no longer receives the
// signals it registered for: a later signal reaches nobody's shutdown.
func TestRunGraceful_SignalRegistrationRemovedOnReturn(t *testing.T) {
	var registered chan<- os.Signal
	restore := bootstrap.SetNotifySignalsForTest(func(c chan<- os.Signal) {
		registered = c
		signal.Notify(c, syscall.SIGUSR1)
	})
	defer restore()

	if err := bootstrap.Run(context.Background(), testObs, bootstrap.Func(func(context.Context) error { return nil })); err != nil {
		t.Fatal(err)
	}

	// Our own registration keeps SIGUSR1 from terminating the test binary,
	// and tells us when the signal has been delivered.
	ours := make(chan os.Signal, 1)
	signal.Notify(ours, syscall.SIGUSR1)
	defer signal.Stop(ours)
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ours:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGUSR1 was not delivered")
	}
	// Delivery fans out to every registered channel at once, so a
	// supervisor channel still registered holds the signal by now.
	time.Sleep(50 * time.Millisecond)
	if n := len(registered); n != 0 {
		t.Errorf("the returned supervisor's channel received %d signal(s); its registration was not removed", n)
	}
}

// The resource closers run after a component FAILED too, not only after a
// clean stop: a boot failure still closes the pools opened before it.
func TestRunGraceful_ClosersRunWhenAComponentFailed(t *testing.T) {
	var closed atomic.Int32
	closer := func(context.Context) error { closed.Add(1); return nil }
	failing := bootstrap.Func(func(context.Context) error { return errors.New("listen: permission denied") })

	err := bootstrap.RunGraceful(context.Background(), testObs, []func(context.Context) error{closer, closer}, failing)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("RunGraceful = %v, want the component error", err)
	}
	if got := closed.Load(); got != 2 {
		t.Errorf("closers run = %d, want 2", got)
	}
}

// An unparseable deadline is logged and the default applies.
func TestShutdownDeadline_UnparseableIsLoggedAndDefaulted(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	got := bootstrap.ShutdownDeadlineForTest(func(string) string { return "ten" })
	if got != bootstrap.DefaultShutdownDeadline {
		t.Errorf("deadline = %s, want the default %s", got, bootstrap.DefaultShutdownDeadline)
	}
	if !strings.Contains(buf.String(), bootstrap.EnvShutdownDeadline) || !strings.Contains(buf.String(), `"ten"`) {
		t.Errorf("log = %q, want the variable and the value named", buf.String())
	}
}

// The telemetry flush at the end of a run is bounded at 5s: a collector that
// accepts the connection and never answers does not hold the process past
// it.
func TestRun_ObservabilityFlushIsBoundedAtFiveSeconds(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }() // held open, never answered
		}
	}()

	obs := observx.Config{ServiceName: "bootstrap-flush-bound", OTelEndpoint: l.Addr().String(), OTelTLS: "off"}
	traced := bootstrap.Func(func(ctx context.Context) error {
		_, span := otel.Tracer("bootstrap-test").Start(ctx, "pending-at-shutdown")
		span.End()
		return nil
	})
	start := time.Now()
	if err := bootstrap.Run(context.Background(), obs, traced); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took := time.Since(start); took > 7*time.Second {
		t.Fatalf("Run returned after %v with an unresponsive collector, want the flush bounded at ~5s", took.Round(100*time.Millisecond))
	}
}

// Every component may report an error at once — failing or panicking —
// and the supervisor still returns: the reports cannot block each other.
func TestRun_EveryComponentFailingAtOnceDoesNotDeadlock(t *testing.T) {
	const n = 64
	comps := make([]bootstrap.Component, 0, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			comps = append(comps, bootstrap.Func(func(context.Context) error { return errors.New("boot failed") }))
		} else {
			comps = append(comps, bootstrap.Func(func(context.Context) error { panic("boot exploded") }))
		}
	}
	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(context.Background(), testObs, comps...) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run = nil with every component failing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return with every component failing at once")
	}
}

// With no shutdown deadline, resource teardown is still bounded: a closer
// that waits on its context is told to give up after 30s. Takes 30s by
// construction.
func TestRunGraceful_ResourceTeardownIsBoundedAtThirtySeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("30s teardown bound")
	}
	t.Setenv(bootstrap.EnvShutdownDeadline, "0")
	var waited time.Duration
	closer := func(ctx context.Context) error {
		start := time.Now()
		<-ctx.Done()
		waited = time.Since(start)
		return ctx.Err()
	}
	noop := bootstrap.Func(func(context.Context) error { return nil })
	done := make(chan error, 1)
	go func() {
		done <- bootstrap.RunGraceful(context.Background(), testObs, []func(context.Context) error{closer}, noop)
	}()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		t.Fatal("RunGraceful did not return: the closer was never told to give up")
	}
	if waited < 25*time.Second || waited > 35*time.Second {
		t.Errorf("the closer's context ended after %v, want ~30s", waited.Round(time.Second))
	}
}

// Transports drain in parallel on the one shared context: two that each
// need a second to drain take about a second together, not two — so the
// shutdown deadline is spent once, not once per transport.
func TestRun_TransportsDrainInParallel(t *testing.T) {
	draining := func(ctx context.Context) error {
		<-ctx.Done()
		time.Sleep(time.Second)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bootstrap.Run(ctx, testObs, bootstrap.Func(draining), bootstrap.Func(draining), bootstrap.Func(draining))
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
		if took := time.Since(start); took > 1800*time.Millisecond {
			t.Errorf("three 1s drains took %v together, want ~1s (in parallel)", took.Round(100*time.Millisecond))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}
