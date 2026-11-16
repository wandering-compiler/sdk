package distx_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// A business method reached OVER THE WIRE inside another process's
// transaction — the request carries the caller's `w17-tx-id`, and the
// transaction was begun in the caller's process — must hold its events until
// that transaction commits, exactly as it would inside one process: published
// once on the commit, never on a rollback, and not twice when a commit refused
// while a call still held the transaction is retried.
//
// The callee here is what the generated business wrapper does: the method
// writes inside the caller's transaction (forwarding its headers, as an author
// who means to take part does), then hands its event to
// txregistry.DeferUntilCommit with the tier's commit hook — RemoteCommits over
// the coordinator's W17TransactionOutcome, which the generated bundle wires.
// Both transports: a separate business binary (grpc) and a composed one
// (inproc), where the hop moves the caller's metadata to the incoming side and
// hides its in-process scope.

// remoteCallee installs the business method on h and returns the hook it
// parks on.
func remoteCallee(h *harness) {
	hook := distx.NewRemoteCommits(h.outcome)
	h.bizFn = func(ctx context.Context, value string) error {
		in, _ := metadata.FromIncomingContext(ctx)
		out := metadata.Pairs(txregistry.HeaderName, firstOf(in.Get(txregistry.HeaderName)))
		if c := firstOf(in.Get(distx.ConnIDHeader)); c != "" {
			out.Set(distx.ConnIDHeader, c)
		}
		if err := h.write(metadata.NewOutgoingContext(ctx, out), connMain, value); err != nil {
			return err
		}
		emit := func() { h.emit("remote:" + value) }
		if txregistry.DeferUntilCommit(ctx, hook, emit) == txregistry.EmitNow {
			emit()
		}
		return nil
	}
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// settle waits for the callee's watcher goroutine to act on the outcome.
func settle(h *harness, want string, n int) int {
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := 0
		for _, e := range h.emittedNow() {
			if e == want {
				got++
			}
		}
		if got >= n || time.Now().After(deadline) {
			// A little longer, so a duplicate has its chance to show.
			time.Sleep(100 * time.Millisecond)
			got = 0
			for _, e := range h.emittedNow() {
				if e == want {
					got++
				}
			}
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRemoteEmit_PublishedOnceAfterTheCallersCommit(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			h := newHarness(t, tr)
			remoteCallee(h)
			err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
				if err := h.callBiz(ctx, "c1"); err != nil {
					return err
				}
				if contains(h.emittedNow(), "remote:c1") {
					t.Errorf("the callee announced before its caller committed: %v", h.emittedNow())
				}
				return nil
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if n := settle(h, "remote:c1", 1); n != 1 {
				t.Errorf("announced %d times after the commit, want 1: %v", n, h.emittedNow())
			}
			eq(t, "rows", h.rows(connMain), []string{"c1"})
			if tr == "grpc" {
				h.mu.Lock()
				awaits := append([]call(nil), h.awaits...)
				h.mu.Unlock()
				if len(awaits) != 1 || strings.Join(awaits[0].connIDs, ",") != replicaID {
					t.Errorf("the wait did not reach the transaction's replica: %+v", awaits)
				}
			}
		})
	}
}

func TestRemoteEmit_NothingAfterTheCallersRollback(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			h := newHarness(t, tr)
			remoteCallee(h)
			err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
				if err := h.callBiz(ctx, "r1"); err != nil {
					return err
				}
				return errBoom
			})
			if !errors.Is(err, errBoom) {
				t.Fatalf("Run: %v", err)
			}
			if n := settle(h, "remote:r1", 1); n != 0 {
				t.Errorf("a rolled-back transaction's callee announced %d times: %v", n, h.emittedNow())
			}
			eq(t, "rows", h.rows(connMain), nil)
		})
	}
}

// A Commit the coordinator refuses because a call inside the transaction
// still holds it (FailedPrecondition) leaves the transaction open; the retry
// that lands is the one commit, and the callee's event goes out once.
func TestRemoteEmit_ABusyCommitRetriedIsAnnouncedOnce(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			h := newHarness(t, tr, txregistry.WithFinishWait(50*time.Millisecond))
			remoteCallee(h)
			handle, ctx, err := distx.Begin(context.Background(), h.client, mainReq())
			if err != nil {
				t.Fatal(err)
			}
			if err := h.callBiz(ctx, "b1"); err != nil {
				t.Fatal(err)
			}
			// A call still running inside the transaction.
			_, release, err := h.reg.LookupTx(context.Background(), handle.TxID(), connMain)
			if err != nil {
				t.Fatal(err)
			}
			err = handle.Commit(context.Background())
			if status.Code(err) != codes.FailedPrecondition {
				release()
				t.Fatalf("commit while held: %v, want FailedPrecondition", err)
			}
			if n := settle(h, "remote:b1", 1); n != 0 {
				release()
				t.Fatalf("announced %d times on a commit that did not happen", n)
			}
			release()
			if err := handle.Commit(context.Background()); err != nil {
				t.Fatalf("retried commit: %v", err)
			}
			if n := settle(h, "remote:b1", 1); n != 1 {
				t.Errorf("announced %d times after the retried commit, want 1: %v", n, h.emittedNow())
			}
		})
	}
}

// The coordinator answers a transaction that already ended with its outcome
// alone, and one it never held with NotFound.
func TestOutcomeServer_AnEndedAndAnUnknownTransaction(t *testing.T) {
	h := newHarness(t, "grpc")
	handle, _, err := distx.Begin(context.Background(), h.client, mainReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := h.outcome.Await(context.Background(), &distxpb.AwaitRequest{TxId: handle.TxID()})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := st.Recv()
	if err != nil || msg.GetOutcome() != distxpb.TxOutcome_TX_OUTCOME_COMMITTED {
		t.Fatalf("ended transaction: %v %v, want COMMITTED", msg, err)
	}
	if _, err := st.Recv(); err == nil {
		t.Error("the stream did not end after the outcome")
	}
	st, err = h.outcome.Await(context.Background(), &distxpb.AwaitRequest{TxId: "never"})
	if err == nil {
		_, err = st.Recv()
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown transaction: %v, want NotFound", err)
	}
	// And the hook drops the work it cannot tie to a transaction, saying so.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(txregistry.HeaderName, "never"))
	if got := txregistry.DeferUntilCommit(ctx, distx.NewRemoteCommits(h.outcome), func() { t.Error("ran") }); got != txregistry.EmitDropped {
		t.Errorf("unknown transaction outcome %v, want dropped", got)
	}
}

// The register bound covers only the coordinator's first answer (PENDING):
// once the wait is registered, a transaction that stays open far longer than
// the bound and then commits still announces — the wait outlives the bound,
// and lasts as long as the transaction does (its own tx timeout ends it).
func TestRemoteEmit_ATransactionLongerThanTheRegisterBoundIsAnnounced(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			defer distx.SetRegisterWait(50 * time.Millisecond)()
			h := newHarness(t, tr)
			remoteCallee(h)
			err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
				if err := h.callBiz(ctx, "long1"); err != nil {
					return err
				}
				time.Sleep(300 * time.Millisecond) // six times the bound
				return nil
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if n := settle(h, "remote:long1", 1); n != 1 {
				t.Errorf("a commit landing after the register bound announced %d times, want 1: %v", n, h.emittedNow())
			}
		})
	}
}

// A process shutting down waits for the events it parked on another
// process's transaction, as it waits for the work a local commit released
// (txregistry.WaitAfterCommit, called before the bus and the coordinator
// connection are closed): otherwise a commit landing during the drain finds
// the bus closed, or the wait is cut with the connection, and the event of a
// committed write is lost.
func TestRemoteEmit_TheShutdownDrainWaitsForAParkedEvent(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) {
			h := newHarness(t, tr)
			remoteCallee(h)
			handle, ctx, err := distx.Begin(context.Background(), h.client, mainReq())
			if err != nil {
				t.Fatal(err)
			}
			if err := h.callBiz(ctx, "d1"); err != nil {
				t.Fatal(err)
			}
			short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			err = txregistry.WaitAfterCommit(short)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("the drain returned %v while an event still waited for its transaction, want it to wait", err)
			}
			if err := handle.Commit(context.Background()); err != nil {
				t.Fatalf("commit: %v", err)
			}
			long, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := txregistry.WaitAfterCommit(long); err != nil {
				t.Fatalf("drain after the commit: %v", err)
			}
			if n := count(h.emittedNow(), "remote:d1"); n != 1 {
				t.Errorf("when the drain returned the event was announced %d times, want 1: %v", n, h.emittedNow())
			}
		})
	}
}

func count(xs []string, want string) int {
	n := 0
	for _, x := range xs {
		if x == want {
			n++
		}
	}
	return n
}
