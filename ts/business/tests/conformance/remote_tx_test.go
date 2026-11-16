package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// A business method reached over the wire inside a transaction its CALLER
// runs (the request carries the caller's `w17-tx-id`; the tier opened none of
// it) holds its event until that transaction commits, asking the coordinator
// (W17TransactionOutcome.Await) — and a rollback drops it. The caller here is
// the harness itself, beginning and ending the transaction on the coordinator
// the tier talks to.
func TestARemoteTransactionHoldsTheEventUntilItCommits(t *testing.T) {
	for _, tc := range []struct {
		text   string
		commit bool
	}{{"remote-committed", true}, {"remote-rolled-back", false}} {
		t.Run(tc.text, func(t *testing.T) {
			mark := env.eventCount()
			id, err := env.reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := timeout(gatewayCtx(t, "w17-tx-id", id), 5*time.Second)
			defer cancel()
			if _, err := env.call(ctx, "CreateNote", set(env.msg("CreateNoteRequest"), map[string]any{"text": tc.text}), "Note"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(400 * time.Millisecond)
			if got := noteCreated(t, env.eventsSince(mark)); len(got) != 0 {
				t.Fatalf("events %v arrived while the caller's transaction was still open", got)
			}
			if tc.commit {
				if err := env.reg.Commit(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				if got := noteCreated(t, waitEvents(mark, 2, 5*time.Second)); !slices.Equal(got, []string{tc.text}) {
					t.Errorf("events after the commit = %v, want [%s]", got, tc.text)
				}
				time.Sleep(300 * time.Millisecond)
				if got := noteCreated(t, env.eventsSince(mark)); len(got) != 1 {
					t.Errorf("events after the commit = %v, want exactly one", got)
				}
				return
			}
			if err := env.reg.Rollback(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			time.Sleep(500 * time.Millisecond)
			if got := noteCreated(t, env.eventsSince(mark)); len(got) != 0 {
				t.Errorf("events of a rolled-back transaction: %v", got)
			}
		})
	}
}
