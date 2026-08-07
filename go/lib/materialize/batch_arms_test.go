package materialize_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/materialize"
)

// Coverage for the FetchBatches edge arms + Guard.Limit non-nil path
// the existing suite doesn't drive. Reuses `ids` / `identity` from
// batch_test.go.

// TestFetchBatches_BatchSizeZeroDefaultsToAll covers the
// `batchSize < 1` clamp — a zero batch size collapses every id into a
// single window.
func TestFetchBatches_BatchSizeZeroDefaultsToAll(t *testing.T) {
	got, err := materialize.FetchBatches(context.Background(), nil, ids(5), 0 /* batchSize */, 1, identity)
	if err != nil {
		t.Fatalf("FetchBatches: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("expected all 5 ids in one window, got %d", len(got))
	}
}

// TestFetchBatches_SequentialError covers the sequential-path error
// return (maxParallel == 1) — the first failing window aborts the run.
func TestFetchBatches_SequentialError(t *testing.T) {
	boom := errors.New("boom")
	fn := func(_ context.Context, _ []int) ([]int, error) { return nil, boom }
	_, err := materialize.FetchBatches(context.Background(), nil, ids(3), 1, 1 /* sequential */, fn)
	if !errors.Is(err, boom) {
		t.Errorf("expected the fn error to propagate, got %v", err)
	}
}

// TestFetchBatches_PreCancelledContextNoLaunch covers the
// parallel-path launch guard: when the context is already cancelled,
// the loop breaks before launching any window goroutine — and the run
// surfaces the cancellation rather than returning a silent empty/partial
// result. (The sequential path already returns context.Canceled through
// fn; the parallel path must match so a cancelled run is never mistaken
// for a complete one.)
func TestFetchBatches_PreCancelledContextNoLaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	called := false
	fn := func(_ context.Context, b []int) ([]int, error) { called = true; return b, nil }

	got, err := materialize.FetchBatches(ctx, nil, ids(4), 1 /* one id/window */, 4 /* parallel */, fn)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("pre-cancelled launch: expected context.Canceled, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no results, got %d", len(got))
	}
	if called {
		t.Error("fn must not run when the context is already cancelled")
	}
}

// TestGuard_Limit covers the non-nil Limit accessor (the existing
// suite only exercises the nil-receiver path).
func TestGuard_Limit(t *testing.T) {
	if got := materialize.NewGuard(42).Limit(); got != 42 {
		t.Errorf("Limit() = %d, want 42", got)
	}
}
