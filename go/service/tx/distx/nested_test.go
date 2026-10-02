package distx_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// Every case runs on both transports unless it is about one of them.
func forEachTransport(t *testing.T, fn func(t *testing.T, h *harness)) {
	for _, tr := range transports {
		t.Run(tr, func(t *testing.T) { fn(t, newHarness(t, tr)) })
	}
}

// assertAffinity: every storage call carried exactly ONE tx id (the given
// one) and exactly one conn id, the replica's — the proxy routes it to the
// replica holding the transaction, and storage adopts the right one.
func assertAffinity(t *testing.T, h *harness, txID string) {
	t.Helper()
	for _, c := range h.storageCalls() {
		if len(c.txIDs) != 1 || c.txIDs[0] != txID {
			t.Errorf("storage call %q carried w17-tx-id %v, want exactly [%s]", c.value, c.txIDs, txID)
		}
		if len(c.connIDs) != 1 || c.connIDs[0] != replicaID {
			t.Errorf("storage call %q carried w17-conn-id %v, want exactly [%s]", c.value, c.connIDs, replicaID)
		}
	}
}

// ---------------------------------------------------------------------------
// The baseline, so every nested case below has something to differ from.

func TestNested_FlatRunCommitsAndRoutesEveryCall(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		var txID string
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			txID = outgoingTxID(t, ctx)
			if err := h.write(ctx, connMain, "a"); err != nil {
				return err
			}
			return h.write(ctx, connMain, "b")
		})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "rows", h.rows(connMain), []string{"a", "b"})
		assertAffinity(t, h, txID)
		if h.count("/Begin") != 1 || h.count("/Commit") != 1 || h.count("/Rollback") != 0 {
			t.Errorf("begin/commit/rollback = %d/%d/%d", h.count("/Begin"), h.count("/Commit"), h.count("/Rollback"))
		}
	})
}

func outgoingTxID(t *testing.T, ctx context.Context) string {
	t.Helper()
	md, _ := metadata.FromOutgoingContext(ctx)
	ids := md.Get(txregistry.HeaderName)
	if len(ids) != 1 {
		t.Fatalf("tx ctx carries %d w17-tx-id values: %v", len(ids), ids)
	}
	return ids[0]
}

// ---------------------------------------------------------------------------
// Joining.

// The defect this exists for: a second Begin appended a second w17-tx-id.
// Nested now means ONE transaction: one Begin, one Commit, every write in it
// carrying the one id, and the inner writes committed with the outer ones.
func TestNested_InnerRunJoinsTheOuterTransaction(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		var outerID, innerID string
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			outerID = outgoingTxID(t, ctx)
			if err := h.write(ctx, connMain, "outer-1"); err != nil {
				return err
			}
			if err := distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				innerID = outgoingTxID(t, ctx)
				return h.write(ctx, connMain, "inner")
			}); err != nil {
				return err
			}
			return h.write(ctx, connMain, "outer-2")
		})
		if err != nil {
			t.Fatal(err)
		}
		if innerID != outerID {
			t.Errorf("inner tx %q, outer tx %q — the inner did not join", innerID, outerID)
		}
		eq(t, "rows", h.rows(connMain), []string{"outer-1", "inner", "outer-2"})
		assertAffinity(t, h, outerID)
		if h.count("/Begin") != 1 || h.count("/Commit") != 1 {
			t.Errorf("begin/commit = %d/%d, want 1/1 — a nested Begin must not reach the server", h.count("/Begin"), h.count("/Commit"))
		}
		for _, v := range []string{"outer-1", "inner", "outer-2"} {
			if adopted, seen := h.adoptedFor(connMain, v); !seen || !adopted {
				t.Errorf("write %q ran outside the transaction (adopted=%v seen=%v)", v, adopted, seen)
			}
		}
	})
}

// Three levels deep is still one transaction.
func TestNested_ThreeLevelsAreOneTransaction(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		var ids []string
		var level func(ctx context.Context, depth int) error
		level = func(ctx context.Context, depth int) error {
			return distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				ids = append(ids, outgoingTxID(t, ctx))
				if err := h.write(ctx, connMain, fmt.Sprintf("d%d", depth)); err != nil {
					return err
				}
				if depth < 3 {
					return level(ctx, depth+1)
				}
				return nil
			})
		}
		if err := level(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] {
			t.Errorf("tx ids per level %v, want one id three times", ids)
		}
		eq(t, "rows", h.rows(connMain), []string{"d1", "d2", "d3"})
		if h.count("/Begin") != 1 || h.count("/Commit") != 1 {
			t.Errorf("begin/commit = %d/%d", h.count("/Begin"), h.count("/Commit"))
		}
	})
}

// Manual Begin/Commit (not Run) nests the same way.
func TestNested_ManualBeginJoinsAndItsCommitIsTheRoots(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		outer, octx, err := distx.Begin(context.Background(), h.client, mainReq())
		if err != nil {
			t.Fatal(err)
		}
		inner, ictx, err := distx.Begin(octx, h.client, mainReq())
		if err != nil {
			t.Fatal(err)
		}
		if inner.TxID() != outer.TxID() || inner.ConnID() != outer.ConnID() || inner.ConnID() != replicaID {
			t.Errorf("joined handle tx/conn %q/%q, root %q/%q", inner.TxID(), inner.ConnID(), outer.TxID(), outer.ConnID())
		}
		if err := h.write(ictx, connMain, "x"); err != nil {
			t.Fatal(err)
		}
		if err := inner.Commit(ictx); err != nil {
			t.Fatal(err)
		}
		// The joined commit committed nothing: the row is not durable yet.
		if got := h.rows(connMain); len(got) != 0 {
			t.Fatalf("a joined Commit made the write durable before the root committed: %v", got)
		}
		if h.count("/Commit") != 0 {
			t.Fatalf("a joined Commit reached the server")
		}
		if err := outer.Commit(octx); err != nil {
			t.Fatal(err)
		}
		eq(t, "rows", h.rows(connMain), []string{"x"})
	})
}

// ---------------------------------------------------------------------------
// Failure in every position.

var errBoom = errors.New("boom")

// The inner method fails and the outer passes the failure on: everything is
// rolled back, the inner's writes with the outer's.
func TestNested_InnerFailurePropagatedRollsBackEverything(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			_ = h.write(ctx, connMain, "outer")
			return distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				_ = h.write(ctx, connMain, "inner")
				return errBoom
			})
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if got := h.rows(connMain); len(got) != 0 {
			t.Errorf("rows survived the rollback: %v", got)
		}
		if h.count("/Commit") != 0 || h.count("/Rollback") != 1 {
			t.Errorf("commit/rollback = %d/%d, want 0/1", h.count("/Commit"), h.count("/Rollback"))
		}
	})
}

// The trap a join creates: the inner fails, the outer SWALLOWS it and
// returns nil. Without savepoints the inner's half cannot be undone alone, so
// committing would keep a write the inner refused. The root rolls back and
// says why.
func TestNested_InnerFailureSwallowedRollsBackAndSaysSo(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			_ = h.write(ctx, connMain, "outer")
			_ = distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				_ = h.write(ctx, connMain, "inner-half-done")
				return errBoom
			})
			return nil // swallowed
		})
		if !errors.Is(err, distx.ErrRollbackOnly) {
			t.Fatalf("err = %v, want ErrRollbackOnly", err)
		}
		if got := h.rows(connMain); len(got) != 0 {
			t.Errorf("a refused inner write was committed: %v", got)
		}
		if h.count("/Commit") != 0 || h.count("/Rollback") != 1 {
			t.Errorf("commit/rollback = %d/%d, want 0/1", h.count("/Commit"), h.count("/Rollback"))
		}
	})
}

// The outer fails AFTER the inner succeeded: the inner's writes go too.
func TestNested_OuterFailureAfterInnerSuccessRollsBackTheInner(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			if err := distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				return h.write(ctx, connMain, "inner")
			}); err != nil {
				return err
			}
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("err = %v", err)
		}
		if got := h.rows(connMain); len(got) != 0 {
			t.Errorf("the inner's write survived the outer's rollback: %v", got)
		}
	})
}

// A panic in the inner method unwinds through both levels, rolls the one
// transaction back, and is still a panic for the caller.
func TestNested_InnerPanicRollsBackAndStillPanics(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		func() {
			defer func() {
				if p := recover(); p != "inner-panic" {
					t.Errorf("recovered %v, want the inner's panic", p)
				}
			}()
			_ = distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
				_ = h.write(ctx, connMain, "outer")
				return distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
					_ = h.write(ctx, connMain, "inner")
					panic("inner-panic")
				})
			})
		}()
		if got := h.rows(connMain); len(got) != 0 {
			t.Errorf("rows survived a panic: %v", got)
		}
		if h.count("/Rollback") != 1 || h.count("/Commit") != 0 {
			t.Errorf("commit/rollback = %d/%d, want 0/1", h.count("/Commit"), h.count("/Rollback"))
		}
	})
}

// ---------------------------------------------------------------------------
// What a nested Begin refuses.

// A transaction on ANOTHER connection inside an open one is not atomic with
// it; refused, and the outer is left exactly as it was — its own work still
// commits if it chooses to carry on.
func TestNested_AnotherConnectionIsRefusedAndTheOuterIsUntouched(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		var nestedErr error
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			if err := h.write(ctx, connMain, "outer"); err != nil {
				return err
			}
			nestedErr = distx.Run(ctx, h.client, &distxpb.BeginRequest{ConnectionName: connAudit}, func(ctx context.Context) error {
				t.Error("the refused nested transaction ran its function")
				return nil
			})
			return nil
		})
		if !errors.Is(nestedErr, distx.ErrNestedMismatch) || !strings.Contains(nestedErr.Error(), connAudit) {
			t.Fatalf("nested Begin on another connection: %v", nestedErr)
		}
		if err != nil {
			t.Fatalf("the refusal leaked into the outer transaction: %v", err)
		}
		eq(t, "main rows", h.rows(connMain), []string{"outer"})
		if got := h.rows(connAudit); len(got) != 0 {
			t.Errorf("audit rows: %v", got)
		}
		if h.count("/Begin") != 1 {
			t.Errorf("begin = %d, want 1 — the refused Begin must not reach the server", h.count("/Begin"))
		}
	})
}

// An explicit isolation a running transaction does not have is refused; an
// unspecified one, or the same one, joins.
func TestNested_IsolationMustMatchOrBeUnspecified(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		outerReq := &distxpb.BeginRequest{ConnectionName: connMain, Isolation: distxpb.Isolation_READ_COMMITTED}
		err := distx.Run(context.Background(), h.client, outerReq, func(ctx context.Context) error {
			for _, iso := range []distxpb.Isolation{distxpb.Isolation_ISOLATION_UNSPECIFIED, distxpb.Isolation_READ_COMMITTED} {
				if err := distx.Run(ctx, h.client, &distxpb.BeginRequest{ConnectionName: connMain, Isolation: iso}, func(context.Context) error { return nil }); err != nil {
					t.Errorf("isolation %s should join: %v", iso, err)
				}
			}
			err := distx.Run(ctx, h.client, &distxpb.BeginRequest{ConnectionName: connMain, Isolation: distxpb.Isolation_SERIALIZABLE}, func(context.Context) error {
				t.Error("ran under a weaker isolation than it asked for")
				return nil
			})
			if !errors.Is(err, distx.ErrNestedMismatch) || !strings.Contains(err.Error(), "SERIALIZABLE") {
				t.Errorf("stricter nested isolation: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// ---------------------------------------------------------------------------
// Contexts outliving their transaction.

// Two Runs in sequence on the same parent are two transactions; a context kept
// past the end of its transaction opens a NEW one and carries only the new id
// (the old append-based attach sent both, and storage adopted the old one).
func TestNested_AFinishedTransactionsContextOpensAFreshOne(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		var kept context.Context
		var firstID string
		if err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			kept, firstID = ctx, outgoingTxID(t, ctx)
			return h.write(ctx, connMain, "first")
		}); err != nil {
			t.Fatal(err)
		}
		var secondID string
		if err := distx.Run(kept, h.client, mainReq(), func(ctx context.Context) error {
			secondID = outgoingTxID(t, ctx) // fails the test on a duplicate header
			return h.write(ctx, connMain, "second")
		}); err != nil {
			t.Fatal(err)
		}
		if secondID == firstID || secondID == "" {
			t.Errorf("second transaction reused id %q", secondID)
		}
		eq(t, "rows", h.rows(connMain), []string{"first", "second"})
		if h.count("/Begin") != 2 || h.count("/Commit") != 2 {
			t.Errorf("begin/commit = %d/%d, want 2/2", h.count("/Begin"), h.count("/Commit"))
		}
	})
}
