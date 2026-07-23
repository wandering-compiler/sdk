package stress

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/e2e/internal/runtime"
	"github.com/wandering-compiler/sdk/go/tooling/e2e/runner"
)

// Run executes one stress preset: the setup steps once (to establish the
// auth token + any shared fixture state), then the load ops under the
// plan's concurrency until the stop condition, then the teardown steps
// once. It returns the measured Report. The error is non-nil only on a
// setup failure (the load phase can't start) — a teardown failure is
// logged but does not void the metrics, and individual request errors are
// counted into the Report, not returned.
//
// callers drives the setup/teardown phases (the ordinary asserting
// transport callers, perf-irrelevant — they run once); lc drives the load
// phase (the fasthttp fast path). load ops carrying an auth-required
// endpoint reuse the token the setup captured as `auth.token`.
func Run(ctx context.Context, label string, plan Plan, setup []runner.Step, load []Op, teardown []runner.Step, callers map[string]runner.Caller, lc *LoadCaller) (*Report, error) {
	run := runtime.NewRun()
	scope := run.NewScope()

	if len(setup) > 0 {
		if err := runner.RunSteps(ctx, scope, setup, callers); err != nil {
			return nil, fmt.Errorf("stress %s: setup: %w", label, err)
		}
	}
	if len(load) == 0 {
		return nil, fmt.Errorf("stress %s: no load ops", label)
	}

	base := scope.Captures()
	token := ""
	if v, ok := scope.Get("auth.token"); ok {
		token = fmt.Sprint(v)
	}

	results := runLoad(ctx, run, plan, load, base, token, lc)

	if len(teardown) > 0 {
		// Best-effort: a fresh scope (the load workers consumed seq
		// counters but teardown re-runs setup-style steps; it can re-auth
		// via its own pre: if needed). Failure is logged, not fatal.
		if err := runner.RunSteps(ctx, run.NewScope(), teardown, callers); err != nil {
			fmt.Fprintf(os.Stderr, "stress %s: teardown failed (metrics still reported): %v\n", label, err)
		}
	}

	return newReport(label, plan.Mode, plan.Concurrency,
		results.lat, results.success, results.conn, results.c4, results.c5, results.elapsed), nil
}

// loadResult is the merged outcome of the worker pool.
type loadResult struct {
	lat                   []int64
	success, conn, c4, c5 int
	elapsed               time.Duration
}

// workerBuf is one worker's lock-free accumulator (merged at the end —
// avoids the single-atomic-index contention a shared buffer would have).
type workerBuf struct {
	lat                   []int64
	success, conn, c4, c5 int
}

// loadState is the shared load-phase context handed to each worker (the
// plan, the ops, the seed captures, the cursors + the stop predicate), so
// a worker takes only its id + buffer.
type loadState struct {
	plan        Plan
	load        []Op
	base        map[string]any
	token       string
	lc          *LoadCaller
	rr          *atomic.Int64 // round-robin cursor (pool mode)
	warmupUntil time.Time
	stop        func() bool
}

func runLoad(ctx context.Context, run *runtime.Run, plan Plan, load []Op, base map[string]any, token string, lc *LoadCaller) loadResult {
	bufs := make([]workerBuf, plan.Concurrency)

	var iter atomic.Int64 // reserved iterations (total mode)
	var rr atomic.Int64

	start := time.Now()
	deadline := start.Add(plan.Duration) // zero Duration → deadline == start (unused; total mode drives)

	st := &loadState{
		plan: plan, load: load, base: base, token: token, lc: lc, rr: &rr,
		warmupUntil: start.Add(plan.Warmup),
		// stop reports whether to keep going: either the shared iteration
		// budget is exhausted (total mode) or the wall-clock window elapsed
		// (duration mode). Reserving via iter.Add splits the total evenly
		// across workers without overshoot.
		stop: func() bool {
			if ctx.Err() != nil {
				return true
			}
			if plan.Duration > 0 {
				return time.Now().After(deadline)
			}
			return iter.Add(1) > int64(plan.TotalRequests)
		},
	}

	var wg sync.WaitGroup
	for w := 0; w < plan.Concurrency; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			st.worker(ctx, run, id, &bufs[id])
		}(w)
	}
	wg.Wait()
	end := time.Now()

	// Measured window excludes the warm-up.
	measuredStart := start
	if plan.Warmup > 0 && st.warmupUntil.Before(end) {
		measuredStart = st.warmupUntil
	}
	res := loadResult{elapsed: end.Sub(measuredStart)}
	for i := range bufs {
		res.lat = append(res.lat, bufs[i].lat...)
		res.success += bufs[i].success
		res.conn += bufs[i].conn
		res.c4 += bufs[i].c4
		res.c5 += bufs[i].c5
	}
	return res
}

// worker runs one load worker until the stop predicate trips: it ramps in,
// then loops firing ops (one per iteration in pool mode, the whole sequence
// in sequence mode) with optional think-time between iterations.
func (st *loadState) worker(ctx context.Context, run *runtime.Run, id int, buf *workerBuf) {
	// Ramp: stagger this worker's start across the ramp window so a cold
	// pool isn't thundering-herded.
	if st.plan.Ramp > 0 && st.plan.Concurrency > 1 {
		offset := time.Duration(int64(st.plan.Ramp) * int64(id) / int64(st.plan.Concurrency))
		select {
		case <-ctx.Done():
			return
		case <-time.After(offset):
		}
	}

	ws := run.NewScope()
	ws.Seed(st.base)
	ws.Capture("worker", id)

	for !st.stop() {
		if st.plan.Mode == ModeSequence {
			for i := range st.load {
				fireOp(ws, &st.load[i], st.token, st.lc, buf, st.warmupUntil)
			}
		} else { // pool
			fireOp(ws, &st.load[st.pickIdx()], st.token, st.lc, buf, st.warmupUntil)
		}
		if st.plan.ThinkTime > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(st.plan.ThinkTime):
			}
		}
	}
}

// pickIdx selects a load op index in pool mode. round_robin uses the shared
// cursor (even spread across ops); random uses a global draw.
func (st *loadState) pickIdx() int {
	n := len(st.load)
	if n == 1 {
		return 0
	}
	if st.plan.Selection == SelectRandom {
		return rand.IntN(n)
	}
	return int((st.rr.Add(1) - 1) % int64(n))
}

// fireOp expands one op's input, fires it through the load caller, and
// records latency + status into the worker buffer when past warm-up.
func fireOp(ws *runtime.Scope, op *Op, token string, lc *LoadCaller, buf *workerBuf, warmupUntil time.Time) {
	expanded, err := runtime.Expand(op.Input, ws)
	if err != nil {
		// An input that can't expand is a malformed op, not load — count
		// it as a connection-class error so it surfaces, don't abort.
		if !time.Now().Before(warmupUntil) {
			buf.conn++
			buf.lat = append(buf.lat, 0)
		}
		return
	}
	input, _ := expanded.(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	tok := ""
	if op.Endpoint.AuthRequired {
		tok = token
	}

	t0 := time.Now()
	code, _ := lc.Fire(op.Endpoint, input, tok, op.Headers)
	lat := time.Since(t0).Nanoseconds()

	// Discard warm-up samples (let the pool scale up first).
	if t0.Before(warmupUntil) {
		return
	}
	buf.lat = append(buf.lat, lat)
	switch {
	case code < 0:
		buf.conn++
	case code >= 500:
		buf.c5++
	case code >= 400:
		buf.c4++
	default:
		buf.success++
	}
}
