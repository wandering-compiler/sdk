package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// failureWaves sends waves of attempts that are all in flight together and
// all fail, one token's refill apart, and returns how many were admitted.
// admit takes one attempt and returns its failure charge.
func failureWaves(l *Limiter, now *time.Time, admit func() (bool, func())) int {
	total := 0
	for wave := 0; wave < 5; wave++ {
		var (
			mu      sync.Mutex
			charges []func()
			wg      sync.WaitGroup
		)
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ok, c := admit(); ok {
					mu.Lock()
					charges = append(charges, c)
					mu.Unlock()
				}
			}()
		}
		wg.Wait() // every attempt was admitted (or not) before any failed
		for _, c := range charges {
			c()
		}
		total += len(charges)
		*now = now.Add(2 * time.Second) // 30/min: one token refills
	}
	return total
}

// Attempts that are in flight at the same time must each hold a token, or a
// caller multiplies its budget by its concurrency: admitting on a look at the
// bucket and charging after the failure let every concurrent attempt in.
func TestChargeable_ConcurrentFailuresStayWithinTheBudget(t *testing.T) {
	const want = DefaultBurst + 4 // the burst, then one refill per later wave

	for name, admit := range map[string]func(*Limiter) (bool, func()){
		"Reserve": func(l *Limiter) (bool, func()) {
			ok, done := l.Reserve(ctxFrom("198.51.100.7:1"))
			return ok, func() { done(true) }
		},
		"ReserveRequest": func(l *Limiter) (bool, func()) {
			ok, done := l.ReserveRequest(reqFrom("198.51.100.7:1"))
			return ok, func() { done(true) }
		},
		"AllowChargeable": func(l *Limiter) (bool, func()) {
			return l.AllowChargeable(ctxFrom("198.51.100.7:1"))
		},
		"AllowChargeableRequest": func(l *Limiter) (bool, func()) {
			return l.AllowChargeableRequest(reqFrom("198.51.100.7:1"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1_000_000, 0)
			l := New(Config{PerMinute: 30, Burst: DefaultBurst})
			l.now = func() time.Time { return now } // advanced only between waves
			got := failureWaves(l, &now, func() (bool, func()) { return admit(l) })
			if got > want {
				t.Fatalf("%d failed attempts admitted from 100-wide waves; the budget is %d", got, want)
			}
		})
	}
}

// done(false) gives the token back, done(true) keeps it, and only the first
// call counts.
func TestReserve_SettlesOnce(t *testing.T) {
	l := New(Config{PerMinute: 6, Burst: 2})
	l.now = frozenClock()
	ctx := ctxFrom("203.0.113.9:1")
	for i := 0; i < 20; i++ {
		ok, done := l.Reserve(ctx)
		if !ok {
			t.Fatalf("success %d refused: a success kept its token", i)
		}
		done(false)
		done(false) // a second refund must not mint a token
	}
	if got := l.tokens("203.0.113.9"); got != 2 {
		t.Fatalf("bucket holds %v after refunds, want the burst (2)", got)
	}
	_, d1 := l.Reserve(ctx)
	d1(true)
	d1(false) // ignored: already settled as a failure
	_, d2 := l.Reserve(ctx)
	d2(true)
	if ok, _ := l.Reserve(ctx); ok {
		t.Fatal("two failures did not drain a burst of two")
	}

	var nilL *Limiter
	if ok, done := nilL.Reserve(ctx); !ok {
		t.Error("nil limiter must admit")
	} else {
		done(true)
	}
	if ok, done := nilL.ReserveRequest(reqFrom("203.0.113.9:1")); !ok {
		t.Error("nil limiter must admit")
	} else {
		done(true)
	}
}

// The older API cannot hear a success, so the token comes back when the
// call's context ends without a charge — and never, for a context that
// cannot end. A charge after that refund takes the token again.
func TestAllowChargeable_RefundFollowsTheContext(t *testing.T) {
	l := New(Config{PerMinute: 6, Burst: 2})
	l.now = frozenClock()

	if ok, _ := l.AllowChargeable(ctxFrom("203.0.113.1:1")); !ok {
		t.Fatal("first attempt refused")
	}
	if got := l.tokens("203.0.113.1"); got != 1 {
		t.Fatalf("an uncancellable call holds no token: bucket at %v, want 1", got)
	}

	ctx, cancel := context.WithCancel(ctxFrom("203.0.113.2:1"))
	_, charge := l.AllowChargeable(ctx)
	cancel()
	waitTokens(t, l, "203.0.113.2", 2)
	charge()
	if got := l.tokens("203.0.113.2"); got != 1 {
		t.Fatalf("a charge after the refund did not take the token again: bucket at %v, want 1", got)
	}
	charge()
	if got := l.tokens("203.0.113.2"); got != 1 {
		t.Fatalf("a repeated charge spent again: bucket at %v, want 1", got)
	}
}
