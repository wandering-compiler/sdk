package materialize_test

import (
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/materialize"
)

func TestGuard_UnderLimit_NoError(t *testing.T) {
	g := materialize.NewGuard(1000)
	for i := 0; i < 10; i++ {
		if err := g.Add(99); err != nil {
			t.Fatalf("Add under limit returned error: %v", err)
		}
	}
	if g.Count() != 990 {
		t.Fatalf("Count = %d, want 990", g.Count())
	}
}

func TestGuard_CrossingLimit_ResourceExhausted(t *testing.T) {
	g := materialize.NewGuard(100)
	if err := g.Add(100); err != nil {
		t.Fatalf("Add at exactly limit must not trip: %v", err)
	}
	err := g.Add(1)
	if err == nil {
		t.Fatal("Add crossing limit returned nil, want RESOURCE_EXHAUSTED")
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("status code = %v, want ResourceExhausted", got)
	}
}

func TestGuard_ZeroLimit_Unbounded(t *testing.T) {
	g := materialize.NewGuard(0)
	if err := g.Add(1 << 30); err != nil {
		t.Fatalf("zero-limit Guard must never trip: %v", err)
	}
	if g.Count() != 1<<30 {
		t.Fatalf("Count = %d, want %d", g.Count(), 1<<30)
	}
}

func TestGuard_NilReceiver_NoOp(t *testing.T) {
	var g *materialize.Guard
	if err := g.Add(1 << 40); err != nil {
		t.Fatalf("nil Guard.Add returned error: %v", err)
	}
	if g.Count() != 0 {
		t.Fatalf("nil Guard.Count = %d, want 0", g.Count())
	}
	if g.Limit() != 0 {
		t.Fatalf("nil Guard.Limit = %d, want 0", g.Limit())
	}
}

// TestGuard_Concurrent pins the cross-goroutine ceiling: the
// bounded-parallel fetch path shares one Guard across batch workers,
// so the limit must hold under concurrent Add. With a limit of 500
// and 10 workers each adding 100 (1000 total), at least one Add must
// trip — and the final count must equal the full attempted total
// (every increment applied regardless of verdict).
func TestGuard_Concurrent(t *testing.T) {
	g := materialize.NewGuard(500)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		tripped int
	)
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Add(100); err != nil {
				mu.Lock()
				tripped++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if tripped == 0 {
		t.Fatal("expected at least one Add to trip the shared ceiling")
	}
	if g.Count() != 1000 {
		t.Fatalf("Count = %d, want 1000 (every increment applied)", g.Count())
	}
}
