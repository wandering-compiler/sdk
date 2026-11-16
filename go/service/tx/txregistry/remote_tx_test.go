package txregistry_test

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// remoteHook records what DeferUntilCommit hands a RemoteCommitHook.
type remoteHook struct {
	txID string
	ctx  context.Context
	out  txregistry.DeferOutcome
}

func (r *remoteHook) OnCommit(string, func()) bool { panic("OnCommit: DeferRemote must be preferred") }

func (r *remoteHook) DeferRemote(ctx context.Context, txID string, _ func()) txregistry.DeferOutcome {
	r.ctx, r.txID = ctx, txID
	return r.out
}

// A tier with no registry of its own (the business tier) is handed a
// RemoteCommitHook: a request carrying another process's `w17-tx-id` parks
// there, with the request context (its routing token) and the hook's answer.
// The old behaviour — announce at once and only report it — is gone.
func TestDeferUntilCommit_ARemoteTransactionParksOnTheRemoteHook(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(txregistry.HeaderName, "tx-remote-1"))
	for _, want := range []txregistry.DeferOutcome{txregistry.EmitDeferred, txregistry.EmitNow, txregistry.EmitDropped} {
		h := &remoteHook{out: want}
		if got := txregistry.DeferUntilCommit(ctx, h, func() {}); got != want {
			t.Errorf("outcome %v, want the hook's %v", got, want)
		}
		if h.txID != "tx-remote-1" || h.ctx != ctx {
			t.Errorf("the hook got tx %q and ctx %v, want the request's", h.txID, h.ctx)
		}
	}
	// No transaction on the request: the hook is not asked.
	h := &remoteHook{out: txregistry.EmitDropped}
	if got := txregistry.DeferUntilCommit(context.Background(), h, func() {}); got != txregistry.EmitNow || h.txID != "" {
		t.Errorf("a request with no transaction: %v (hook asked for %q)", got, h.txID)
	}
	// No hook at all: nothing to park on, announced now, and nothing said.
	if got := txregistry.DeferUntilCommit(ctx, nil, func() {}); got != txregistry.EmitNow {
		t.Errorf("no hook: %v, want EmitNow", got)
	}
	if buf.Len() != 0 {
		t.Errorf("reported:\n%s", buf.String())
	}
}

func awaitMemory(t *testing.T, opts ...txregistry.MemoryOption) *txregistry.Memory {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return txregistry.NewMemory(map[string]*sql.DB{"main": db}, opts...)
}

func recvEnded(t *testing.T, ch <-chan txregistry.Ended) (txregistry.Ended, bool) {
	t.Helper()
	select {
	case e := <-ch:
		return e, true
	case <-time.After(200 * time.Millisecond):
		return txregistry.EndedUnknown, false
	}
}

// AwaitEnd: one answer per waiter, when the transaction settles — committed,
// rolled back, timed out — and never on a commit refused while a handler
// holds the transaction.
func TestMemory_AwaitEndAnswersOnceWhenTheTransactionSettles(t *testing.T) {
	ctx := context.Background()
	t.Run("commit", func(t *testing.T) {
		m := awaitMemory(t)
		id, _ := m.Begin(ctx, txregistry.BeginOptions{ConnectionName: "main"})
		ch, _, ok := m.AwaitEnd(id)
		if !ok {
			t.Fatal("open transaction not found")
		}
		if _, got := recvEnded(t, ch); got {
			t.Fatal("answered while open")
		}
		if err := m.Commit(ctx, id); err != nil {
			t.Fatal(err)
		}
		if e, _ := recvEnded(t, ch); e != txregistry.EndedCommitted {
			t.Errorf("got %v, want committed", e)
		}
		// Settled: a later waiter gets the answer at once.
		ch2, _, ok := m.AwaitEnd(id)
		if e, _ := recvEnded(t, ch2); !ok || e != txregistry.EndedCommitted {
			t.Errorf("late waiter: %v %v", e, ok)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		m := awaitMemory(t)
		id, _ := m.Begin(ctx, txregistry.BeginOptions{ConnectionName: "main"})
		ch, _, _ := m.AwaitEnd(id)
		if err := m.Rollback(ctx, id); err != nil {
			t.Fatal(err)
		}
		if e, _ := recvEnded(t, ch); e != txregistry.EndedNotCommitted {
			t.Errorf("got %v, want not committed", e)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		m := awaitMemory(t)
		id, _ := m.Begin(ctx, txregistry.BeginOptions{ConnectionName: "main", Timeout: 20 * time.Millisecond})
		ch, _, _ := m.AwaitEnd(id)
		select {
		case e := <-ch:
			if e != txregistry.EndedNotCommitted {
				t.Errorf("got %v, want not committed", e)
			}
		case <-time.After(2 * time.Second):
			t.Error("the orphan watcher's rollback was not answered")
		}
	})
	t.Run("busy commit then retry", func(t *testing.T) {
		m := awaitMemory(t, txregistry.WithFinishWait(30*time.Millisecond))
		id, _ := m.Begin(ctx, txregistry.BeginOptions{ConnectionName: "main"})
		ch, _, _ := m.AwaitEnd(id)
		_, release, err := m.LookupTx(ctx, id, "main")
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Commit(ctx, id); err == nil {
			t.Fatal("commit while held succeeded")
		}
		if e, got := recvEnded(t, ch); got {
			t.Fatalf("a refused commit answered %v", e)
		}
		release()
		if err := m.Commit(ctx, id); err != nil {
			t.Fatal(err)
		}
		if e, _ := recvEnded(t, ch); e != txregistry.EndedCommitted {
			t.Errorf("got %v, want committed", e)
		}
		if e, got := recvEnded(t, ch); got {
			t.Errorf("answered twice: %v", e)
		}
	})
	t.Run("unknown and withdrawn", func(t *testing.T) {
		m := awaitMemory(t)
		if _, _, ok := m.AwaitEnd("never"); ok {
			t.Error("an id never held was found")
		}
		id, _ := m.Begin(ctx, txregistry.BeginOptions{ConnectionName: "main"})
		_, cancel, _ := m.AwaitEnd(id)
		cancel()
		cancel()
		if err := m.Commit(ctx, id); err != nil {
			t.Fatal(err)
		}
	})
}
