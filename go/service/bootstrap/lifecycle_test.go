package bootstrap_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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
