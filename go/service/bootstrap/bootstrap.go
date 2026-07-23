// Package bootstrap is the wandering-compiler's process supervisor:
// the one entrypoint a generated `main.go` calls to run its
// transport(s). It owns the cross-cutting lifecycle every binary
// otherwise re-rolls by hand:
//
//   - observability: a single [observx.MustInit] for the whole
//     process (+ a bounded flush on the way down);
//   - OS signals: one SIGINT/SIGTERM handler that cancels a shared
//     context — components never install their own;
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
	"fmt"
	"os/signal"
	"runtime/debug"
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
// Standalone binaries pass no resources (Run) — each single component
// already owns its own teardown inside Run (e.g. runtime.GRPCComponent
// closes DB pools in its post-GracefulStop shutdown hooks), so the
// ordering is intra-component there and needs no phase here.
func RunGraceful(parent context.Context, obs observx.Config, resources []func(context.Context) error, comps ...Component) error {
	if err := observx.MustInit(obs); err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = observx.Shutdown(flushCtx)
	}()

	// One signal owner for the whole process; the derived ctx is the
	// single shutdown source every component selects on.
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	wg.Wait()
	close(errCh)

	// Phase 2 — resource teardown. Every component has fully returned,
	// so all transports have drained their in-flight work; only now is
	// it safe to close the shared resources they depended on. The
	// process ctx is already cancelled, so use a fresh bounded context.
	if len(resources) > 0 {
		drainCtx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		for _, closeRes := range resources {
			if closeRes != nil {
				_ = closeRes(drainCtx)
			}
		}
		dcancel()
	}

	var first error
	for err := range errCh {
		if first == nil {
			first = err
		}
	}
	return first
}
