package distx_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txscope"
)

func TestOnCommit_OutsideATransactionRunsAtOnce(t *testing.T) {
	ran := false
	if out := distx.OnCommit(context.Background(), func() { ran = true }); out != txscope.NoScope {
		t.Errorf("outcome = %v, want NoScope", out)
	}
	if !ran {
		t.Error("fn did not run outside a transaction")
	}
}

func TestOnCommit_WaitsForTheCommit(t *testing.T) {
	tx := &scriptedTx{}
	var seen []string
	err := distx.Run(context.Background(), tx, mainReq(), func(ctx context.Context) error {
		if out := distx.OnCommit(ctx, func() { seen = append(seen, "hook:"+tx.seq()) }); out != txscope.Deferred {
			t.Errorf("outcome = %v, want Deferred", out)
		}
		if len(seen) != 0 {
			t.Error("fn ran before the commit")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hook:begin,commit"}; !slices.Equal(seen, want) {
		t.Errorf("seen = %v, want %v — the hook runs once, after the commit call", seen, want)
	}
}

func TestOnCommit_DroppedOnRollback(t *testing.T) {
	tx := &scriptedTx{}
	ran := false
	boom := errors.New("refused after the write")
	err := distx.Run(context.Background(), tx, mainReq(), func(ctx context.Context) error {
		distx.OnCommit(ctx, func() { ran = true })
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if ran {
		t.Error("fn ran for a transaction that rolled back")
	}
	if tx.seq() != "begin,rollback" {
		t.Errorf("calls = %s", tx.seq())
	}
}

func TestOnCommit_DroppedWhenTheCommitFails(t *testing.T) {
	tx := &scriptedTx{commitErr: errors.New("commit refused")}
	ran := false
	_ = distx.Run(context.Background(), tx, mainReq(), func(ctx context.Context) error {
		distx.OnCommit(ctx, func() { ran = true })
		return nil
	})
	if ran {
		t.Error("fn ran although the commit failed")
	}
}

// A joined (nested) Run commits nothing: its hook waits for the OUTER commit,
// and is dropped with the outer transaction when the outer one rolls back
// after the inner one succeeded.
func TestOnCommit_InANestedRunWaitsForTheOuterCommit(t *testing.T) {
	for _, outerFails := range []bool{false, true} {
		tx := &scriptedTx{}
		ran := false
		_ = distx.Run(context.Background(), tx, mainReq(), func(ctx context.Context) error {
			if err := distx.Run(ctx, tx, mainReq(), func(ctx context.Context) error {
				distx.OnCommit(ctx, func() { ran = true })
				return nil
			}); err != nil {
				return err
			}
			if ran {
				t.Error("the inner hook ran when the inner (joined) Run returned")
			}
			if outerFails {
				return errors.New("outer refuses")
			}
			return nil
		})
		if ran == outerFails {
			t.Errorf("outerFails=%v: ran=%v", outerFails, ran)
		}
	}
}

func TestOnCommit_AfterTheTransactionEndedIsNotParked(t *testing.T) {
	tx := &scriptedTx{}
	var kept context.Context
	_ = distx.Run(context.Background(), tx, mainReq(), func(ctx context.Context) error {
		kept = ctx
		return nil
	})
	ran := false
	if out := distx.OnCommit(kept, func() { ran = true }); out != txscope.AlreadyCommitted || !ran {
		t.Errorf("after commit: outcome=%v ran=%v, want AlreadyCommitted and run", out, ran)
	}

	tx = &scriptedTx{}
	_ = distx.Run(context.Background(), tx, mainReq(), func(ctx context.Context) error {
		kept = ctx
		return errors.New("no")
	})
	ran = false
	if out := distx.OnCommit(kept, func() { ran = true }); out != txscope.Discarded || ran {
		t.Errorf("after rollback: outcome=%v ran=%v, want Discarded and not run", out, ran)
	}
}

func TestOnCommit_NilFnIsANoop(t *testing.T) {
	if out := distx.OnCommit(context.Background(), nil); out != txscope.NoScope {
		t.Errorf("outcome = %v", out)
	}
}

// A caller that hung up gets no commit (D3.4), so its hook never runs.
func TestOnCommit_DroppedForACallerThatHungUp(t *testing.T) {
	tx := &scriptedTx{}
	ctx, cancel := context.WithCancel(context.Background())
	ran := false
	_ = distx.Run(ctx, tx, mainReq(), func(txCtx context.Context) error {
		distx.OnCommit(txCtx, func() { ran = true })
		cancel()
		return nil
	})
	if ran {
		t.Error("fn ran for a caller that hung up before the commit")
	}
	if tx.seq() != "begin,rollback" {
		t.Errorf("calls = %s, want begin,rollback", tx.seq())
	}
}

// A commit whose outcome cannot be settled runs no hook.
func TestOnCommit_NotRunWhenTheOutcomeIsUnknown(t *testing.T) {
	tx := &scriptedTx{
		commitErr:   status.Error(codes.Unavailable, "connection reset"),
		rollbackErr: status.Error(codes.NotFound, "unknown tx"),
	}
	ran := false
	err := distx.Run(context.Background(), tx, mainReq(), func(ctx context.Context) error {
		distx.OnCommit(ctx, func() { ran = true })
		return nil
	})
	if !errors.Is(err, distx.ErrCommitOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrCommitOutcomeUnknown", err)
	}
	if ran {
		t.Error("fn ran for a commit whose outcome is unknown")
	}
}

// Across a hop to another business method the callee is NOT inside its
// caller's transaction — over the wire by construction (context values do not
// cross it), and over a composed binary's in-process bridge because the hop
// detaches the scope: OnCommit on the handler's own context runs at once.
// Only the context the callee's own Run hands it defers.
func TestOnCommit_ACalleeAcrossAnInProcessHopIsOutsideTheCallersTransaction(t *testing.T) {
	h := newHarness(t, "inproc")
	var out txscope.Outcome
	ran := false
	h.bizFn = func(ctx context.Context, _ string) error {
		out = distx.OnCommit(ctx, func() { ran = true })
		return nil
	}
	err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
		if err := h.callBiz(ctx, "hop"); err != nil {
			return err
		}
		if !ran {
			t.Error("the callee's hook waited for the caller's commit")
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if out != txscope.NoScope {
		t.Errorf("outcome = %v, want NoScope", out)
	}
}
