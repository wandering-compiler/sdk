package bootstrap_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/service/bootstrap"
)

// stuck is a component that ignores its context until release is closed —
// a transport whose drain hangs.
func stuck(release <-chan struct{}) bootstrap.Component {
	return bootstrap.Func(func(context.Context) error {
		<-release
		return nil
	})
}

// The shutdown deadline bounds the wait for components: one that never
// observes the cancel no longer keeps the process alive.
func TestRun_ShutdownDeadlineBoundsAStuckComponent(t *testing.T) {
	t.Setenv("W17_SHUTDOWN_DEADLINE_SECONDS", "1")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(ctx, testObs, stuck(release)) }()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "W17_SHUTDOWN_DEADLINE_SECONDS") {
			t.Fatalf("Run = %v, want an error naming the shutdown deadline", err)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("Run took %s to give up on a 1s deadline", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run waited for a component that never stops")
	}
}

// The deadline covers the resource teardown too, and a closer still
// running when it passes sees its context cancelled.
func TestRunGraceful_ShutdownDeadlineBoundsResourceTeardown(t *testing.T) {
	t.Setenv("W17_SHUTDOWN_DEADLINE_SECONDS", "1")
	closerCtxDone := make(chan struct{})
	closer := func(ctx context.Context) error {
		<-ctx.Done()
		close(closerCtxDone)
		return ctx.Err()
	}
	comp := bootstrap.Func(func(ctx context.Context) error { <-ctx.Done(); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bootstrap.RunGraceful(ctx, testObs, []func(context.Context) error{closer}, comp)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunGraceful = nil although the teardown overran the deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunGraceful did not bound the resource teardown")
	}
	select {
	case <-closerCtxDone:
	case <-time.After(2 * time.Second):
		t.Error("the overrunning closer's context was not cancelled")
	}
}

// A component error that arrived before the deadline passed is still
// returned alongside the deadline error.
func TestRun_ShutdownDeadlineKeepsTheComponentError(t *testing.T) {
	t.Setenv("W17_SHUTDOWN_DEADLINE_SECONDS", "1")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	boom := errors.New("listener failed")
	failing := bootstrap.Func(func(context.Context) error { return boom })

	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(context.Background(), testObs, failing, stuck(release)) }()
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("Run = %v, want it to carry %v", err, boom)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run waited for a component that never stops")
	}
}

// 0 is the explicit opt-out: the wait is unbounded again.
func TestRun_ShutdownDeadlineZeroIsUnbounded(t *testing.T) {
	t.Setenv("W17_SHUTDOWN_DEADLINE_SECONDS", "0")
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bootstrap.Run(ctx, testObs, stuck(release)) }()
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned %v before the component stopped, with the bound off", err)
	case <-time.After(1500 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Run = %v after the component stopped cleanly", err)
	}
}

// The default fits a 30s orchestrator grace period with the 5s flush.
func TestDefaultShutdownDeadline_FitsTheCommonGracePeriod(t *testing.T) {
	if bootstrap.DefaultShutdownDeadline+5*time.Second >= 30*time.Second {
		t.Errorf("default deadline %s plus the 5s flush does not end before a 30s grace period", bootstrap.DefaultShutdownDeadline)
	}
}

// A second SIGTERM during the drain ends the process at once instead of
// being swallowed. Runs the supervisor in a child process and signals it.
func TestRun_SecondSignalExitsImmediately(t *testing.T) {
	if os.Getenv("BOOTSTRAP_SIGNAL_CHILD") == "1" {
		signalChild()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRun_SecondSignalExitsImmediately$")
	cmd.Env = append(os.Environ(), "BOOTSTRAP_SIGNAL_CHILD=1", "W17_SHUTDOWN_DEADLINE_SECONDS=0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := bufio.NewScanner(stdout)
	waitFor := func(want string) {
		t.Helper()
		got := make(chan bool, 1)
		go func() {
			for lines.Scan() {
				if lines.Text() == want {
					got <- true
					return
				}
			}
			got <- false
		}()
		select {
		case ok := <-got:
			if !ok {
				t.Fatalf("child ended before printing %q", want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("child did not print %q", want)
		}
	}
	waitFor("running")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor("draining")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Errorf("child exit = %v, want status 1", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second SIGTERM during the drain did not end the process")
	}
}

// signalChild runs a supervisor whose component never finishes draining.
func signalChild() {
	comp := bootstrap.Func(func(ctx context.Context) error {
		fmt.Println("running")
		<-ctx.Done()
		fmt.Println("draining")
		time.Sleep(time.Hour) // a drain that never ends
		return nil
	})
	_ = bootstrap.Run(context.Background(), testObs, comp)
	os.Exit(0)
}
