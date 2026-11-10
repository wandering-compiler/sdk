package restgw

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// E16.1 on the store's own clock: a ticket is redeemable until its TTL
// passes and indistinguishable from an unknown one afterwards.
func TestTicket_TTLExpiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := NewMemoryTicketStore()
	defer s.Close()
	s.now = func() time.Time { return now }
	ctx := context.Background()

	ok, _ := s.Issue(ctx, nil, 30*time.Second)
	gone, _ := s.Issue(ctx, nil, 30*time.Second)
	now = now.Add(30 * time.Second) // at the deadline: still valid
	if _, err := s.Redeem(ctx, ok); err != nil {
		t.Fatalf("Redeem at the deadline: %v", err)
	}
	now = now.Add(time.Nanosecond)
	if _, err := s.Redeem(ctx, gone); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("Redeem past the TTL: %v, want ErrTicketInvalid", err)
	}
	if _, err := s.Redeem(ctx, gone); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("an expired ticket must be gone after the attempt: %v", err)
	}
}

// One ticket, many concurrent redemptions: exactly one wins, every other
// one gets ErrTicketInvalid.
func TestTicket_ConcurrentRedeemsAdmitOne(t *testing.T) {
	s := NewMemoryTicketStore()
	defer s.Close()
	ctx := context.Background()
	for round := 0; round < 50; round++ {
		tk, err := s.Issue(ctx, map[string]string{"k": "v"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		var wins, invalid atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := s.Redeem(ctx, tk)
				switch {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, ErrTicketInvalid):
					invalid.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins.Load() != 1 || invalid.Load() != 7 {
			t.Fatalf("round %d: %d redemptions won, %d got ErrTicketInvalid; want 1 and 7", round, wins.Load(), invalid.Load())
		}
	}
}

// A full store sweeps expired tickets before refusing, in expiry order
// whatever order the TTLs were issued in, and the sweep costs what it
// removes — not a walk over every live ticket on each refused Issue.
func TestMemoryTicketStore_FullStoreSweepCostsOnlyWhatExpired(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := NewMemoryTicketStore()
	defer s.Close()
	s.now = func() time.Time { return now }
	ctx := context.Background()

	// Mixed TTLs, issued out of expiry order.
	long, _ := s.Issue(ctx, nil, time.Hour)
	short, _ := s.Issue(ctx, nil, time.Second)
	mid, _ := s.Issue(ctx, nil, time.Minute)
	now = now.Add(2 * time.Second)
	s.reapExpired(now)
	s.mu.Lock()
	_, l := s.entries[long]
	_, sh := s.entries[short]
	_, m := s.entries[mid]
	s.mu.Unlock()
	if !l || sh || !m {
		t.Fatalf("after the reap: long=%v short=%v mid=%v; want only short gone", l, sh, m)
	}

	// Fill to the ceiling with live tickets, then hammer the full store.
	for i := 2; i < DefaultMaxTickets; i++ {
		if _, err := s.Issue(ctx, nil, time.Hour); err != nil {
			t.Fatalf("filling at %d: %v", i, err)
		}
	}
	begin := time.Now()
	for i := 0; i < 5000; i++ {
		if _, err := s.Issue(ctx, nil, time.Hour); !errors.Is(err, ErrTicketStoreFull) {
			t.Fatalf("Issue on a full store: %v", err)
		}
	}
	if elapsed := time.Since(begin); elapsed > 200*time.Millisecond {
		t.Fatalf("5000 refusals on a full store of live tickets took %v; the sweep must not walk every ticket", elapsed)
	}
}
