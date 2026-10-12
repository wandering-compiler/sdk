package materialize_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/materialize"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// Inside a transaction the windows are fetched one after another — a
// transaction is one connection — while outside one the same call runs them
// in parallel.
func TestFetchBatches_InsideATransactionRunsSequentially(t *testing.T) {
	ids := make([]int, 16)
	for i := range ids {
		ids[i] = i
	}
	run := func(ctx context.Context) (maxInFlight int32, got []int) {
		var inFlight, peak atomic.Int32
		out, err := materialize.FetchBatches(ctx, nil, ids, 2, 4, func(_ context.Context, b []int) ([]int, error) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			return b, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return peak.Load(), out
	}

	if peak, _ := run(context.Background()); peak < 2 {
		t.Fatalf("outside a transaction the fetch never ran in parallel (peak %d); the test proves nothing", peak)
	}
	for name, ctx := range map[string]context.Context{
		"adopted distributed transaction": metadata.NewIncomingContext(context.Background(), metadata.Pairs(txregistry.HeaderName, "tx-1")),
		"caller-marked transaction":       materialize.WithinTx(context.Background()),
	} {
		peak, got := run(ctx)
		if peak != 1 {
			t.Errorf("%s: %d windows in flight at once on one transaction", name, peak)
		}
		for i, v := range got {
			if v != i {
				t.Fatalf("%s: results out of order: %v", name, got)
			}
		}
	}
}
