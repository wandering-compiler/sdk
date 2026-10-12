// Package bootstrap is the wandering-compiler's process supervisor:
// the one entrypoint a generated `main.go` calls to run its
// transport(s). It owns the cross-cutting lifecycle every binary
// otherwise re-rolls by hand:
//
//   - observability: a single [observx.MustInit] for the whole
//     process (+ a bounded flush on the way down);
//   - OS signals: one SIGINT/SIGTERM/SIGHUP handler that cancels a
//     shared context — components never install their own; a second
//     signal during the drain exits at once;
//   - a deadline on the whole shutdown (component drain + resource
//     teardown), so the process ends on its own terms before the
//     orchestrator's kill;
//   - composition: N [Component]s run concurrently, each on the
//     shared context; the first to exit (clean or error) cancels
//     the context so the siblings drain too.
//
// The shape is deliberate: a binary is "a set of Components run by
// one supervisor". Standalone binaries pass ONE component; the
// single-binary build passes several (storage + gateway + …) with
// no change to the component code. A component is anything that
// blocks until its context is cancelled, then shuts down
// gracefully — exactly what a gRPC server, an HTTP server, or an
// eventbus subscriber loop already is.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// Component is one long-running unit of a process. Run blocks until
// ctx is cancelled (the supervisor's stop signal), then shuts down
// gracefully and returns — nil on a clean ctx-driven stop, or the
// terminal error if the component failed (e.g. a listener bind).
//
// A component must NOT init observability or install its own signal
// handler; the supervisor owns both. Cleanup belongs inside Run
// (deferred teardown that runs as Run returns on ctx cancellation).
type Component interface {
	Run(ctx context.Context) error
}

// Func adapts a bare `func(context.Context) error` into a
// [Component] — handy for the `Serve(ctx)`-shaped entrypoints
// (rest.Serve, mcp.Serve via a closure, dispatch.RunSubscribers, …).
type Func func(context.Context) error

// Run satisfies [Component].
func (f Func) Run(ctx context.Context) error { return f(ctx) }

// Run is the process entrypoint. It initialises observability once
// from obs, installs a single SIGINT/SIGTERM handler that cancels a
// context derived from parent, runs every component concurrently on
// that context, and blocks until they all return. The first
// component to exit (for any reason) cancels the shared context so
// the rest shut down too; Run then returns the first non-nil
// component error (nil when every component stopped cleanly).
//
// observability is flushed (bounded) after all components drain,
// even when one failed to start.
//
// Shutdown is bounded as a whole (see [EnvShutdownDeadline]): once it
// starts, the components and the resource teardown must finish within
// the deadline, or Run returns an error naming it without waiting for
// them. A second SIGINT/SIGTERM/SIGHUP while shutting down exits the
// process immediately with status 1.
func Run(parent context.Context, obs observx.Config, comps ...Component) error {
	return RunGraceful(parent, obs, nil, comps...)
}

// RunGraceful is [Run] plus an ordered resource-teardown phase. It runs
// every component concurrently exactly as Run does, but once they have
// ALL returned — i.e. each transport component has observed the shared
// ctx cancel and finished its own graceful stop, so no request is still
// in flight — it invokes each closer in `resources` (in order), bounded
// by a fresh 30s teardown context.
//
// This gives a COMPOSED binary the "drain transport → THEN close shared
// resources" ordering. A single-binary build folds several transports
// (REST + MCP + rpc + …) that all reach the tiers' DB pools by direct
// in-process call; those pools outlive the transports and must be closed
// only after every transport has drained. Passing the pool closers as
// `resources` (rather than as a peer Component that races the transports
// on the same ctx cancel) guarantees a slow in-flight request never
// loses its DB pool mid-flight. Ordering within `resources` is
// caller-defined — close dependents before their dependencies (e.g. the
// business tier before the storage tier it calls).
//
// A component whose bounded stop abandoned handlers that were still
// running says so with [HoldResources]; the closers are then skipped and
// the skip is reported, because closing a resource under a running request
// is the failure this phase exists to prevent.
//
// Standalone binaries pass no resources (Run) — each single component
// already owns its own teardown inside Run (e.g. runtime.GRPCComponent
// closes DB pools in its post-GracefulStop shutdown hooks), so the
// ordering is intra-component there and needs no phase here.
func RunGraceful(parent context.Context, obs observx.Config, resources []func(context.Context) error, comps ...Component) error {
	deadline := shutdownDeadline(os.Getenv)
	// A hold, not a plain init: a second supervisor in the same process
	// (a test binary, a composed binary) shares the setup when its
	// Config is the same and is refused when it differs, instead of
	// swapping the providers under the first one's running components.
	// The last supervisor to finish flushes and shuts down.
	releaseObs, err := observx.Acquire(obs)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// observx is going away, so its own failure (spans or events
		// that did not reach the collector) can only go to the log.
		if err := releaseObs(flushCtx); err != nil {
			log.Printf("bootstrap: observability flush: %v", err)
		}
	}()

	// One signal owner for the whole process; the derived ctx is the
	// single shutdown source every component selects on.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	sigCh := make(chan os.Signal, 2)
	notifySignals(sigCh)
	defer signal.Stop(sigCh)
	go watchSignals(sigCh, cancel, finished)
	gate := &teardownGate{}
	ctx = context.WithValue(ctx, teardownGateKey{}, gate)

	var wg sync.WaitGroup
	errCh := make(chan error, len(comps))
	for _, c := range comps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// First component to exit tears down the siblings: a
			// failed boot cancels the shared ctx so the others stop
			// cleanly instead of orphaning the process half-up.
			defer cancel()
			// A panic in any component (a gRPC server lifecycle, an
			// eventbus subscriber loop, a plugin drain) would
			// otherwise crash the whole process — every sibling dies
			// with it. This is the one goroutine every generated
			// binary funnels through, so recovering here converts a
			// component panic into a terminal error + a shared-ctx
			// cancel (sibling drain), and routes the value + stack
			// through observx (Sentry + OTel) — never an os.Exit-style
			// crash. A panic is always "not noise" (quality.md
			// §Sentry).
			defer func() {
				if r := recover(); r != nil {
					err := fmt.Errorf("component panic: %v\n%s", r, debug.Stack())
					observx.ReportError(ctx, err)
					errCh <- err
				}
			}()
			if err := c.Run(ctx); err != nil {
				errCh <- err
			}
		}()
	}

	// teardownCtx is cancelled when the shutdown deadline passes, so a
	// resource closer still running then is told to give up.
	teardownCtx, teardownCancel := context.WithCancel(context.Background())
	defer teardownCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
		close(errCh)

		// Phase 2 — resource teardown. Every component has fully returned,
		// so all transports have drained their in-flight work; only now is
		// it safe to close the shared resources they depended on. The
		// process ctx is already cancelled, so use a fresh bounded context.
		//
		// Unless a component returned with request work still running on
		// resources it could not stop (see HoldResources): closing them then
		// would pull a pool or a bus out from under a handler mid-request.
		// The process is about to exit, which releases them anyway; what is
		// lost by skipping is a clean close, and that is reported.
		if held := gate.reasons(); len(held) > 0 && len(resources) > 0 {
			observx.ReportError(context.Background(), fmt.Errorf("bootstrap: resource teardown skipped, request work still running: %s", strings.Join(held, "; ")))
		} else if len(resources) > 0 {
			drainCtx, dcancel := context.WithTimeout(teardownCtx, 30*time.Second)
			for i, closeRes := range resources {
				if closeRes == nil {
					continue
				}
				// A failing closer does not stop the rest, and it does not
				// change Run's result (the components already decided that);
				// it is reported so a failed pool close or bus flush is seen.
				if err := closeRes(drainCtx); err != nil {
					observx.ReportError(drainCtx, fmt.Errorf("bootstrap: resource closer %d: %w", i, err))
				}
			}
			dcancel()
		}
	}()

	// Shutdown starts when the shared ctx is cancelled (a signal, the
	// parent, or the first component to return); from then on the rest
	// of the sequence has `deadline` to finish.
	select {
	case <-done:
	case <-ctx.Done():
		if err := awaitShutdown(done, deadline); err != nil {
			teardownCancel()
			observx.ReportError(context.Background(), err)
			return errors.Join(firstQueued(errCh), err)
		}
	}

	var first error
	for err := range errCh {
		if first == nil {
			first = err
		}
	}
	return first
}

// EnvShutdownDeadline bounds the whole shutdown sequence — the drain of
// every component and the resource teardown after it — in whole seconds.
// Unset means [DefaultShutdownDeadline]; 0 or a negative value removes the
// bound (wait for everything); an unparseable value is logged and the
// default applies.
const EnvShutdownDeadline = "W17_SHUTDOWN_DEADLINE_SECONDS"

// DefaultShutdownDeadline is the shutdown bound when none is configured.
// Orchestrators commonly kill a process 30s after asking it to stop
// (Kubernetes' default termination grace period); 20s for drain and
// teardown plus at most 5s of telemetry flush ends the process before
// that. Under a shorter grace period (docker stop waits 10s), set
// [EnvShutdownDeadline] lower, and keep each component's own drain bound
// (runtime.Config.ShutdownTimeout, default 10s) below it.
const DefaultShutdownDeadline = 20 * time.Second

func shutdownDeadline(lookup func(string) string) time.Duration {
	raw := lookup(EnvShutdownDeadline)
	if raw == "" {
		return DefaultShutdownDeadline
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("bootstrap: %s=%q is not a whole number of seconds; using %s", EnvShutdownDeadline, raw, DefaultShutdownDeadline)
		return DefaultShutdownDeadline
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// awaitShutdown waits for done, for at most d (d == 0: no bound).
func awaitShutdown(done <-chan struct{}, d time.Duration) error {
	if d <= 0 {
		<-done
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return nil
	case <-t.C:
		return fmt.Errorf("bootstrap: shutdown did not finish within %s (%s); exiting with components or resource closers still running", d, EnvShutdownDeadline)
	}
}

// firstQueued returns the first component error already reported, without
// waiting for the others.
func firstQueued(errCh <-chan error) error {
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// Test seams: the signals the supervisor owns, and how it force-exits.
var (
	notifySignals = func(c chan<- os.Signal) {
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	}
	forceExit = os.Exit
)

// watchSignals cancels the shared context on the first signal and exits the
// process on the second: an operator (or orchestrator) that asks twice
// does not want to wait for the drain.
func watchSignals(sigCh <-chan os.Signal, cancel context.CancelFunc, finished <-chan struct{}) {
	select {
	case sig := <-sigCh:
		log.Printf("bootstrap: received %s; shutting down (send it again to exit immediately)", sig)
		cancel()
	case <-finished:
		return
	}
	select {
	case sig := <-sigCh:
		log.Printf("bootstrap: received %s again during shutdown; exiting immediately", sig)
		forceExit(1)
	case <-finished:
	}
}

// HoldResources tells the supervisor running ctx's component that request
// work it started may still be using the shared resources when the
// component returns, so [RunGraceful] must not close them. reason says what
// is still running; it is reported together with the skipped teardown.
//
// A transport calls it when its bounded stop had to abandon handlers that
// did not return in time (a gRPC server's forced stop does not wait for
// them). Outside a supervisor — ctx not derived from one — it does nothing.
func HoldResources(ctx context.Context, reason string) {
	if g, ok := ctx.Value(teardownGateKey{}).(*teardownGate); ok {
		g.hold(reason)
	}
}

// teardownGate collects the HoldResources calls of one RunGraceful.
type teardownGate struct {
	mu   sync.Mutex
	held []string
}

type teardownGateKey struct{}

func (g *teardownGate) hold(reason string) {
	g.mu.Lock()
	g.held = append(g.held, reason)
	g.mu.Unlock()
}

func (g *teardownGate) reasons() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.held...)
}
