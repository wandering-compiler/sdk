package materialize

import (
	"context"
	"database/sql"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// FetchBatches splits ids into windows of batchSize and runs fn for
// each window, concatenating the per-window results in window order.
// It is the runtime backing for the bounded-parallel LateMaterialize
// fetch (docs/specs/storage/bounded-materialization.md,
// `materialize.max_parallel`).
//
// Concurrency is bounded by `maxParallel`, further clamped to the
// connection pool's headroom (see [effectiveParallel]) so a single
// method can't exhaust the pool. maxParallel <= 1 (or a single
// window) runs sequentially — same observable result, no goroutines.
//
// Ordering: results are assembled in window order regardless of which
// window's fetch completes first, so a parallel run is observationally
// identical to a sequential one. (The LateMaterialize path forbids
// ORDER BY, so within-window row order is already unspecified.)
//
// Errors: the first fn error cancels the shared context and aborts the
// run; in-flight windows observe the cancellation through ctx and the
// error is returned. Partial results are discarded.
//
// fn MUST treat ctx as the cancellation signal for its query and MUST
// NOT mutate shared state — it returns its window's items and
// FetchBatches does the merge.
func FetchBatches[B any, T any](
	ctx context.Context,
	pool *sql.DB,
	ids []B,
	batchSize, maxParallel int,
	fn func(ctx context.Context, batch []B) ([]T, error),
) ([]T, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if batchSize < 1 {
		batchSize = len(ids)
	}

	type window struct{ lo, hi int }
	var windows []window
	for lo := 0; lo < len(ids); lo += batchSize {
		hi := lo + batchSize
		if hi > len(ids) {
			hi = len(ids)
		}
		windows = append(windows, window{lo, hi})
	}

	par := effectiveParallel(maxParallel, pool)
	if par <= 1 || len(windows) <= 1 {
		var out []T
		for _, w := range windows {
			r, err := fn(ctx, ids[w.lo:w.hi])
			if err != nil {
				return nil, err
			}
			out = append(out, r...)
		}
		return out, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([][]T, len(windows))
	sem := make(chan struct{}, par)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for i, w := range windows {
		if ctx.Err() != nil {
			break // a prior window failed; stop launching
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, w window) {
			defer wg.Done()
			defer func() { <-sem }()
			// fn runs generated scan + author-defined Child{Pre,Post}Scan
			// conversions on this DETACHED goroutine — a panic here (bad DB
			// row, nil field, enum/proto cast) would NOT be seen by the gRPC
			// handler's RecoverPanic (different goroutine) and would crash the
			// whole process. Convert it into firstErr so FetchBatches returns
			// a clean error to the handler, mirroring the fn-error path.
			defer func() {
				if r := recover(); r != nil {
					observx.ReportError(ctx, fmt.Errorf("PANIC materialize fetch window [%d:%d]: %v\n%s", w.lo, w.hi, r, debug.Stack()))
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("panic in parallel materialize fetch: %v", r)
						cancel()
					}
					mu.Unlock()
				}
			}()
			if ctx.Err() != nil {
				return
			}
			r, err := fn(ctx, ids[w.lo:w.hi])
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
				return
			}
			results[idx] = r
		}(i, w)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	// Parent cancellation: the launch loop may have broken early (line
	// `break`) and in-flight windows may have short-circuited on ctx
	// without ever calling fn — so firstErr stays nil yet the result set
	// is incomplete. firstErr is only paired with our own cancel(), so a
	// nil firstErr + cancelled ctx means the PARENT cancelled. Surface it
	// rather than silently returning a partial/empty slice (the sequential
	// path already surfaces this through fn).
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var out []T
	for _, r := range results {
		out = append(out, r...)
	}
	return out, nil
}

// effectiveParallel clamps the author's requested concurrency to a
// safe ceiling: at least 1, and — when the pool advertises a finite
// MaxOpenConnections — no more than the pool size minus one reserved
// connection, so a single fan-out method never monopolises the whole
// pool and starves concurrent requests. A nil pool or an unlimited
// pool (MaxOpenConnections == 0) leaves the request honoured as-is.
func effectiveParallel(maxParallel int, pool *sql.DB) int {
	if maxParallel < 1 {
		maxParallel = 1
	}
	if pool == nil {
		return maxParallel
	}
	if mx := pool.Stats().MaxOpenConnections; mx > 0 {
		headroom := mx - 1
		if headroom < 1 {
			headroom = 1
		}
		if maxParallel > headroom {
			maxParallel = headroom
		}
	}
	return maxParallel
}
