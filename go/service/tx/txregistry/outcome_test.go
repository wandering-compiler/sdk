package txregistry_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// The reason a transaction is gone survives it: a Commit that arrives
// after the auto-rollback interceptor discarded the tx is told which
// call did it, not merely that the id is unknown.
//
// This is the shape a consumer hit in the field — a guarded write whose
// `WHERE` matched nothing returned NotFound, the business handler
// deliberately tolerated it, and the Commit three lines later answered
// `unknown tx_id`. Every layer named a symptom; nothing named the
// cause, and the reported diagnosis started at "the transaction
// plumbing is broken".
func TestMemory_RollbackCaused_ExplainsLaterCommit(t *testing.T) {
	reg := singleConnRegistry(t)
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	cause := "a call to /docs.DocumentMutation/UnmarkPaid inside it failed with NotFound"
	if err := reg.RollbackCaused(context.Background(), id, cause); err != nil {
		t.Fatalf("RollbackCaused: %v", err)
	}

	err = reg.Commit(context.Background(), id)
	if !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Fatalf("Commit after auto-rollback = %v, want ErrUnknownTxID", err)
	}
	if !strings.Contains(err.Error(), cause) {
		t.Errorf("Commit error = %q, want it to carry the cause %q", err, cause)
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("Commit error = %q, want it to name the tx id", err)
	}
}

// Every drain path leaves a reason, so the three states a caller
// confuses with each other — committed already, rolled back by
// somebody, timed out — are distinguishable from the error alone.
func TestMemory_Outcome_PerDrainPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drain func(*testing.T, *txregistry.Memory, string)
		want  string
	}{
		{
			name: "committed",
			drain: func(t *testing.T, reg *txregistry.Memory, id string) {
				mustNil(t, reg.Commit(context.Background(), id))
			},
			want: "committed",
		},
		{
			name: "rolled back by the caller",
			drain: func(t *testing.T, reg *txregistry.Memory, id string) {
				mustNil(t, reg.Rollback(context.Background(), id))
			},
			want: "rolled back",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := singleConnRegistry(t)
			id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			tc.drain(t, reg, id)

			// Both surfaces a straggler can reach report it.
			second := reg.Commit(context.Background(), id)
			if !errors.Is(second, txregistry.ErrUnknownTxID) || !strings.Contains(second.Error(), tc.want) {
				t.Errorf("Commit = %v, want ErrUnknownTxID mentioning %q", second, tc.want)
			}
			_, _, look := reg.LookupTx(context.Background(), id, "main")
			if !errors.Is(look, txregistry.ErrUnknownTxID) || !strings.Contains(look.Error(), tc.want) {
				t.Errorf("LookupTx = %v, want ErrUnknownTxID mentioning %q", look, tc.want)
			}
		})
	}
}

// An id this registry never held has no reason to give, and says only
// that — inventing one would be worse than the bare message.
func TestMemory_Outcome_UnknownIDStaysBare(t *testing.T) {
	reg := singleConnRegistry(t)
	err := reg.Commit(context.Background(), "never-existed")
	if !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Fatalf("Commit(unknown) = %v, want ErrUnknownTxID", err)
	}
	if strings.Contains(err.Error(), "rolled back") || strings.Contains(err.Error(), "committed") {
		t.Errorf("Commit(unknown) = %q, want no invented outcome", err)
	}
}

// The reasons are bounded: a process that runs millions of
// transactions must not accumulate one record per id. The oldest are
// evicted, and eviction costs the diagnostic, never correctness — the
// id is still reported unknown.
func TestMemory_Outcome_BoundedByCap(t *testing.T) {
	db := openSQLite(t)
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})

	// sqlite in-memory serialises transactions, so drain each before
	// opening the next.
	const rounds = 400 // comfortably past the 256 cap
	var first string
	for i := range rounds {
		id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
		if err != nil {
			t.Fatalf("Begin %d: %v", i, err)
		}
		if i == 0 {
			first = id
		}
		if err := reg.RollbackCaused(context.Background(), id, fmt.Sprintf("cause-%d", i)); err != nil {
			t.Fatalf("RollbackCaused %d: %v", i, err)
		}
	}

	// The oldest reason is gone; the id is still correctly unknown.
	err := reg.Commit(context.Background(), first)
	if !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Fatalf("Commit(evicted) = %v, want ErrUnknownTxID", err)
	}
	if strings.Contains(err.Error(), "cause-0") {
		t.Errorf("Commit(evicted) = %q, want the oldest reason evicted", err)
	}
	if got := reg.Active(); got != 0 {
		t.Errorf("Active = %d, want 0 — the test leaked transactions", got)
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
}
