package restgw_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// Two gateway replicas each hold their own ticket store: a ticket issued on
// one is not redeemable on the other (the documented per-process limit —
// issue and redeem must reach the same process), and a flood that fills one
// replica's store leaves the other's capacity untouched.
func TestTicketStore_ReplicasAreIndependent(t *testing.T) {
	a := restgw.NewMemoryTicketStore(restgw.WithMaxTickets(2))
	defer a.Close()
	b := restgw.NewMemoryTicketStore(restgw.WithMaxTickets(2))
	defer b.Close()
	ctx := context.Background()

	tk, err := a.Issue(ctx, map[string]string{"user": "u1"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue on A: %v", err)
	}
	if _, err := b.Redeem(ctx, tk); err == nil {
		t.Fatal("a ticket issued on replica A was redeemed on replica B")
	}
	if _, err := a.Issue(ctx, nil, time.Minute); err != nil {
		t.Fatalf("second Issue on A: %v", err)
	}
	if _, err := a.Issue(ctx, nil, time.Minute); !errors.Is(err, restgw.ErrTicketStoreFull) {
		t.Fatalf("third Issue on A = %v, want ErrTicketStoreFull", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := b.Issue(ctx, nil, time.Minute); err != nil {
			t.Fatalf("Issue %d on B while A is full: %v", i, err)
		}
	}
	if got, err := a.Redeem(ctx, tk); err != nil || got["user"] != "u1" {
		t.Fatalf("Redeem on the issuing replica = %v, %v", got, err)
	}
}
