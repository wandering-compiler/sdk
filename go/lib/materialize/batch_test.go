package materialize_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/materialize"
)

func ids(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// identity fn returns each window's elements verbatim, so the merged
// output must equal the input ids in order — the ordering contract,
// whether sequential or parallel.
func identity(_ context.Context, batch []int) ([]int, error) {
	return append([]int(nil), batch...), nil
}

func TestFetchBatches_SequentialOrder(t *testing.T) {
	got, err := materialize.FetchBatches(context.Background(), nil, ids(25), 10, 1, identity)
	if err != nil {
		t.Fatalf("FetchBatches: %v", err)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("out[%d] = %d, want %d (order not preserved)", i, v, i)
		}
	}
	if len(got) != 25 {
		t.Fatalf("len = %d, want 25", len(got))
	}
}

func TestFetchBatches_ParallelPreservesOrder(t *testing.T) {
	// Deterministic rendezvous: the first `par` calls must all be
	// in-flight simultaneously to release the barrier. If FetchBatches
	// ran sequentially this would deadlock the test (caught by the
	// -timeout), proving the parallel branch fired. Later windows see
	// arrived >= par and pass straight through.
	const par = 4
	var (
		mu      sync.Mutex
		arrived int
		cond    = sync.NewCond(&mu)
	)
	fn := func(ctx context.Context, batch []int) ([]int, error) {
		mu.Lock()
		arrived++
		if arrived >= par {
			cond.Broadcast()
		}
		for arrived < par {
			cond.Wait()
		}
		mu.Unlock()
		return append([]int(nil), batch...), nil
	}
	got, err := materialize.FetchBatches(context.Background(), nil, ids(100), 10, par, fn)
	if err != nil {
		t.Fatalf("FetchBatches: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("out[%d] = %d, want %d (parallel reordered results)", i, v, i)
		}
	}
}

func TestFetchBatches_ErrorAborts(t *testing.T) {
	boom := errors.New("boom")
	fn := func(ctx context.Context, batch []int) ([]int, error) {
		if batch[0] == 20 { // the third window (batchSize 10)
			return nil, boom
		}
		return append([]int(nil), batch...), nil
	}
	_, err := materialize.FetchBatches(context.Background(), nil, ids(100), 10, 4, fn)
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom error, got %v", err)
	}
}

// A panic inside fn on the parallel branch must be recovered into an
// error rather than crashing the process — fn runs on a detached
// goroutine the gRPC handler's RecoverPanic cannot see.
func TestFetchBatches_ParallelPanicBecomesError(t *testing.T) {
	fn := func(ctx context.Context, batch []int) ([]int, error) {
		if batch[0] == 20 { // the third window (batchSize 10)
			panic("scan blew up")
		}
		return append([]int(nil), batch...), nil
	}
	_, err := materialize.FetchBatches(context.Background(), nil, ids(100), 10, 4, fn)
	if err == nil {
		t.Fatal("expected an error from a panicking fn, got nil")
	}
	if !strings.Contains(err.Error(), "panic in parallel materialize fetch") {
		t.Fatalf("error %q does not mention the recovered panic", err)
	}
}

func TestFetchBatches_EmptyIDs(t *testing.T) {
	got, err := materialize.FetchBatches(context.Background(), nil, []int{}, 10, 4, identity)
	if err != nil || got != nil {
		t.Fatalf("empty ids: got (%v, %v), want (nil, nil)", got, err)
	}
}

// Single window never spins up goroutines even with maxParallel > 1.
func TestFetchBatches_SingleWindowSequential(t *testing.T) {
	called := 0
	fn := func(ctx context.Context, batch []int) ([]int, error) {
		called++
		return batch, nil
	}
	got, err := materialize.FetchBatches(context.Background(), nil, ids(5), 100, 8, fn)
	if err != nil {
		t.Fatalf("FetchBatches: %v", err)
	}
	if called != 1 || len(got) != 5 {
		t.Fatalf("called=%d len=%d, want 1 / 5", called, len(got))
	}
}

func ExampleFetchBatches() {
	out, _ := materialize.FetchBatches(context.Background(), nil, []int{1, 2, 3, 4, 5}, 2, 1,
		func(_ context.Context, b []int) ([]int, error) { return b, nil })
	fmt.Println(out)
	// Output: [1 2 3 4 5]
}
