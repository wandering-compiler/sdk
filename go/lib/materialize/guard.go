// Package materialize provides runtime safety primitives for the
// query optimizer's app-side materialization paths (LateMaterialize
// / Pipeline — see docs/specs/storage/bounded-materialization.md).
//
// When the optimizer decomposes a JOIN-bearing SELECT into a
// narrow-then-fetch chain, it materializes the intermediate ID set
// in the host process. The codegen-time decision to do so rests on
// a STATIC estimate (`estimated_row_count × selectivity` vs
// `query_memory_budget_bytes`). That estimate can drift from reality
// — an unselective filter, a stale hint, a tenant far larger than
// the rest — and an unbounded materialization then peaks BE memory.
//
// [Guard] turns that unbounded risk into a bounded, typed, recoverable
// failure: the generated fetch loop feeds each streamed intermediate
// row through [Guard.Add]; once the running count crosses the hard
// ceiling, Add returns a gRPC RESOURCE_EXHAUSTED error instead of
// letting the slice grow without limit. This is what makes the
// `query_memory_budget_bytes` "worst case is more round-trips, not
// OOM" promise (proto/w17/module.proto) actually true.
//
// The package is deliberately thin (conventions-global/go.md: no
// heavyweight runtime). Batching itself is emitted as plain Go in the
// generated body; this package only owns the safety counter.
package materialize

import (
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Guard is a fail-closed counter that caps how many intermediate rows
// an optimizer-decomposed query may materialize app-side. A zero
// limit (and the nil Guard) means "no ceiling" — the optimizer could
// not derive a bound, so the count is tracked but never trips. A
// positive limit trips [Guard.Add] with RESOURCE_EXHAUSTED the moment
// the running total would exceed it.
//
// Guard is safe for concurrent use: the bounded-parallel fetch path
// (materialize.max_parallel > 1) shares one Guard across batch
// workers, so the ceiling holds across goroutines rather than
// per-worker. The nil Guard is a valid no-op receiver — generated
// code for an unbounded method may pass nil rather than allocating.
type Guard struct {
	limit uint64 // 0 == unlimited
	count atomic.Uint64
}

// NewGuard returns a Guard enforcing `limit` total intermediate rows.
// limit == 0 disables the ceiling (the Guard still counts, so
// [Guard.Count] reports the materialized total for observability).
func NewGuard(limit uint64) *Guard {
	return &Guard{limit: limit}
}

// Add records that `n` more intermediate rows are about to be
// materialized and returns a RESOURCE_EXHAUSTED error if doing so
// would cross the ceiling. The increment is applied regardless of
// the verdict so [Guard.Count] reflects the attempted total; callers
// abort on a non-nil error and never reach the over-limit rows.
//
// A nil Guard or a zero-limit Guard always returns nil (unbounded).
func (g *Guard) Add(n uint64) error {
	if g == nil {
		return nil
	}
	total := g.count.Add(n)
	if g.limit != 0 && total > g.limit {
		return status.Errorf(codes.ResourceExhausted,
			"query materialized %d intermediate rows, exceeding the %d-row ceiling "+
				"(materialize.max_intermediate_rows / query_memory_budget_bytes); "+
				"narrow the filter or raise the ceiling — see "+
				"docs/specs/storage/bounded-materialization.md",
			total, g.limit)
	}
	return nil
}

// Count returns the number of intermediate rows recorded so far.
// Useful for emitting an observability span attribute after a method
// completes; safe on a nil receiver (returns 0).
func (g *Guard) Count() uint64 {
	if g == nil {
		return 0
	}
	return g.count.Load()
}

// Limit returns the configured ceiling (0 == unlimited). Safe on a
// nil receiver.
func (g *Guard) Limit() uint64 {
	if g == nil {
		return 0
	}
	return g.limit
}
