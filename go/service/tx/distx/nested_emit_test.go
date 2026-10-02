package distx_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// ---------------------------------------------------------------------------
// Business-tier emits — the reason nesting had to learn about commits.

// The consumer's case: a business method composes another one, whose
// `(w17.event_emit)` fires when IT returns. Inside the caller's transaction
// that is too early — the event must wait for the caller's commit.
func TestEmit_ANestedBusinessEmitWaitsForTheRootCommit(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			if err := distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				if err := h.write(ctx, connMain, "batch"); err != nil {
					return err
				}
				h.bizEmit(ctx, "ImportBatchQueued")
				return nil
			}); err != nil {
				return err
			}
			if got := h.emittedNow(); len(got) != 0 {
				t.Errorf("announced before the root committed: %v", got)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !contains(h.emittedNow(), "biz:ImportBatchQueued") {
			t.Errorf("not announced after commit: %v", h.emittedNow())
		}
	})
}

// Rolled back, at any level and for any reason: nothing is announced — not
// the business event, not the storage one.
func TestEmit_NothingIsAnnouncedForARolledBackTransaction(t *testing.T) {
	cases := map[string]func(h *harness, ctx context.Context) error{
		"outer fails": func(h *harness, ctx context.Context) error {
			_ = distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				_ = h.write(ctx, connMain, "x")
				h.bizEmit(ctx, "inner")
				return nil
			})
			h.bizEmit(ctx, "outer")
			return errBoom
		},
		"inner fails, propagated": func(h *harness, ctx context.Context) error {
			h.bizEmit(ctx, "outer")
			return distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				_ = h.write(ctx, connMain, "x")
				h.bizEmit(ctx, "inner")
				return errBoom
			})
		},
		"inner fails, swallowed (rollback-only)": func(h *harness, ctx context.Context) error {
			h.bizEmit(ctx, "outer")
			_ = distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				_ = h.write(ctx, connMain, "x")
				h.bizEmit(ctx, "inner")
				return errBoom
			})
			return nil
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			forEachTransport(t, func(t *testing.T, h *harness) {
				err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error { return fn(h, ctx) })
				if err == nil {
					t.Fatal("a failed transaction reported success")
				}
				if got := h.emittedNow(); len(got) != 0 {
					t.Errorf("announced for a rolled-back transaction: %v", got)
				}
				if got := h.rows(connMain); len(got) != 0 {
					t.Errorf("rows: %v", got)
				}
			})
		})
	}
}

// Outside any transaction a business emit is immediate — the behaviour every
// project has today, unchanged.
func TestEmit_OutsideATransactionIsImmediate(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		h.bizEmit(context.Background(), "now")
		eq(t, "emitted", h.emittedNow(), []string{"biz:now"})
	})
}

// Parked work runs in the order it was parked, across levels.
func TestEmit_ParkedWorkRunsInOrderAcrossLevels(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			h.bizEmit(ctx, "a")
			_ = distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				h.bizEmit(ctx, "b")
				return distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
					h.bizEmit(ctx, "c")
					return nil
				})
			})
			h.bizEmit(ctx, "d")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "emitted", h.emittedNow(), []string{"biz:a", "biz:b", "biz:c", "biz:d"})
	})
}

// Storage-tier emits: each fires exactly once, after the commit. Over the wire
// they park on the storage registry (context values do not cross); in a
// composed binary they park on the business transaction's scope. Either way:
// once, and never before the commit.
func TestEmit_StorageEmitsFireOnceAfterTheCommit(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			if err := h.write(ctx, connMain, "s1"); err != nil {
				return err
			}
			if err := distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				return h.write(ctx, connMain, "s2")
			}); err != nil {
				return err
			}
			if got := h.emittedNow(); len(got) != 0 {
				t.Errorf("storage announced before commit: %v", got)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := h.emittedNow()
		sort.Strings(got)
		eq(t, "emitted", got, []string{"storage:s1", "storage:s2"})
	})
}

// A panic in parked work AFTER the commit: the transaction stays committed,
// Commit reports success, the rest of the parked work still runs, and no
// rollback is sent for a transaction that already landed.
func TestEmit_APanickingHookAfterCommitDoesNotUndoTheCommit(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			if err := h.write(ctx, connMain, "kept"); err != nil {
				return err
			}
			h.bizEmit(ctx, "before")
			if txregistry.DeferUntilCommit(ctx, nil, func() { panic("emit blew up") }) != txregistry.EmitDeferred {
				t.Error("hook was not deferred")
			}
			h.bizEmit(ctx, "after")
			return nil
		})
		if err != nil {
			t.Fatalf("a hook panic turned a commit into an error: %v", err)
		}
		eq(t, "rows", h.rows(connMain), []string{"kept"})
		// The storage write's own announcement too: it parked first.
		eq(t, "emitted", h.emittedNow(), []string{"storage:kept", "biz:before", "biz:after"})
		if h.count("/Rollback") != 0 {
			t.Errorf("a rollback was sent for a committed transaction")
		}
	})
}

// A context kept past its transaction: work parked on it after a COMMIT runs
// immediately; after a ROLLBACK it is dropped.
func TestEmit_AfterTheEndOfTheTransaction(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		var committed, rolledBack context.Context
		_ = distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error { committed = ctx; return nil })
		_ = distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error { rolledBack = ctx; return errBoom })

		if got := txregistry.DeferUntilCommit(committed, nil, func() { h.emit("late-commit") }); got != txregistry.EmitNow {
			t.Errorf("after commit: %v, want EmitNow", got)
		}
		if got := txregistry.DeferUntilCommit(rolledBack, nil, func() { h.emit("late-rollback") }); got != txregistry.EmitDropped {
			t.Errorf("after rollback: %v, want EmitDropped", got)
		}
		if got := h.emittedNow(); len(got) != 0 {
			t.Errorf("a late hook ran by itself: %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Commit failing, or never happening.

// A Commit the server refuses drops the parked work: the write did not land.
func TestEmit_ACommitTheServerRefusesDropsTheParkedWork(t *testing.T) {
	c := &scriptedTx{commitErr: errors.New("commit refused")}
	var emitted []string
	err := distx.Run(context.Background(), c, mainReq(), func(ctx context.Context) error {
		return distx.Run(ctx, c, mainReq(), func(ctx context.Context) error {
			if txregistry.DeferUntilCommit(ctx, nil, func() { emitted = append(emitted, "x") }) != txregistry.EmitDeferred {
				t.Error("not deferred")
			}
			return nil
		})
	})
	if err == nil || !strings.Contains(err.Error(), "commit refused") {
		t.Fatalf("err = %v", err)
	}
	if len(emitted) != 0 {
		t.Errorf("announced a write whose commit failed: %v", emitted)
	}
	if c.seq() != "begin,commit" {
		t.Errorf("calls = %s, want begin,commit — one transaction", c.seq())
	}
}

// Rollback-only, and the rollback ITSELF fails: the caller sees both.
func TestNested_RollbackOnlyWithAFailingRollbackReportsBoth(t *testing.T) {
	c := &scriptedTx{rollbackErr: errors.New("rollback refused")}
	err := distx.Run(context.Background(), c, mainReq(), func(ctx context.Context) error {
		_ = distx.Run(ctx, c, mainReq(), func(context.Context) error { return errBoom })
		return nil
	})
	if !errors.Is(err, distx.ErrRollbackOnly) || !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("err = %v", err)
	}
	if c.seq() != "begin,rollback" {
		t.Errorf("calls = %s, want begin,rollback (never commit)", c.seq())
	}
}

// The root's context cancelled before the commit: nothing commits, nothing is
// announced (Commit refuses on a dead context; the rollback outlives it).
func TestEmit_ACancelledRootAnnouncesNothing(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		ctx, cancel := context.WithCancel(context.Background())
		err := distx.Run(ctx, h.client, mainReq(), func(tctx context.Context) error {
			_ = distx.Run(tctx, h.client, mainReq(), func(tctx context.Context) error {
				_ = h.write(tctx, connMain, "x")
				h.bizEmit(tctx, "x")
				return nil
			})
			cancel()
			return nil
		})
		if err == nil {
			t.Fatal("a cancelled transaction reported success")
		}
		if got := h.emittedNow(); len(got) != 0 {
			t.Errorf("announced: %v", got)
		}
		if got := h.rows(connMain); len(got) != 0 {
			t.Errorf("rows: %v", got)
		}
		// And it was ROLLED BACK, not left open until the server times it out.
		if h.count("/Commit") != 0 || h.count("/Rollback") != 1 {
			t.Errorf("commit/rollback = %d/%d, want 0/1", h.count("/Commit"), h.count("/Rollback"))
		}
	})
}

// A nested Begin asking for a SHORT tx timeout does not shorten the one
// transaction there is: the root's bound governs (a joined Begin opens
// nothing, so there is nothing for its timeout to apply to).
func TestNested_AnInnerTimeoutDoesNotCutTheRootShort(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			return distx.Run(ctx, h.client, &distxpb.BeginRequest{ConnectionName: connMain, TxTimeoutMs: 1}, func(ctx context.Context) error {
				time.Sleep(50 * time.Millisecond)
				return h.write(ctx, connMain, "slow")
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "rows", h.rows(connMain), []string{"slow"})
	})
}

// The commit carries the replica's conn id, so the proxy routes it to the
// replica that holds the transaction — joined or not.
func TestNested_TheCommitIsRoutedToTheTransactionsReplica(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		if err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			return distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				return h.write(ctx, connMain, "x")
			})
		}); err != nil {
			t.Fatal(err)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, c := range h.calls {
			if strings.HasSuffix(c.method, "/Commit") && (len(c.connIDs) != 1 || c.connIDs[0] != replicaID) {
				t.Errorf("Commit carried w17-conn-id %v", c.connIDs)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Topology must not change meaning.

// A business method reached through a TIER HOP inside a transaction behaves
// identically in a composed binary (inprocgrpc) and a split one (the wire):
// the hop is a process boundary, the callee sees the caller's transaction only
// through metadata, and its own distx.Run opens its OWN transaction. The
// in-process hop used to carry the scope too — it would have JOINED here and
// opened its own over the wire, the same code meaning two things depending on
// how it was deployed.
//
// Asserted on inproc against what the wire does by construction (context
// values cannot cross it): the callee committed its own transaction, which the
// caller's rollback does not undo, and announced on that commit.
func TestComposed_AHopInsideATransactionMeansWhatTheWireMeans(t *testing.T) {
	h := newHarness(t, "inproc")
	var calleeID string
	h.bizFn = func(ctx context.Context, value string) error {
		return distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
			calleeID = outgoingTxID(t, ctx)
			if err := h.write(ctx, connMain, value); err != nil {
				return err
			}
			h.bizEmit(ctx, value)
			return nil
		})
	}
	var callerID string
	err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
		callerID = outgoingTxID(t, ctx)
		if err := h.callBiz(ctx, "hop"); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if calleeID == "" || calleeID == callerID {
		t.Fatalf("callee tx %q, caller tx %q — the hop joined instead of opening its own", calleeID, callerID)
	}
	eq(t, "rows (the callee's own committed transaction)", h.rows(connMain), []string{"hop"})
	if !contains(h.emittedNow(), "biz:hop") {
		t.Errorf("the callee's commit was not announced: %v", h.emittedNow())
	}
	if h.count("/Begin") != 2 || h.count("/Commit") != 1 || h.count("/Rollback") != 1 {
		t.Errorf("begin/commit/rollback = %d/%d/%d, want 2/1/1",
			h.count("/Begin"), h.count("/Commit"), h.count("/Rollback"))
	}
}

// A storage handler that opens its own transaction (the auth plugin does) while
// its caller is inside one: same rows and same announcements on both
// transports.
func TestComposed_AStorageHandlersOwnTransactionIsTheSameOnBothTransports(t *testing.T) {
	results := map[string]string{}
	for _, tr := range transports {
		h := newHarness(t, tr)
		h.ownTx = true
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			if err := h.write(ctx, connMain, "own"); err != nil {
				return err
			}
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("%s: err = %v", tr, err)
		}
		got := h.emittedNow()
		sort.Strings(got)
		results[tr] = fmt.Sprintf("rows=%v emitted=%v", h.rows(connMain), got)
	}
	if results["grpc"] != results["inproc"] {
		t.Errorf("the same code meant different things:\n  grpc:   %s\n  inproc: %s", results["grpc"], results["inproc"])
	}
}

// Business code that sets its OWN outgoing metadata replaces the
// transaction's headers on that context. A nested distx.Run on it must put
// them back — joining on whatever outgoing metadata was left would send the
// inner writes with no w17-tx-id: each committed on its own, outside the
// transaction, and kept when the caller rolls back.
func TestNested_AJoinRestoresHeadersTheCallerOverwrote(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-caller", "own-metadata"))
			if err := distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
				md, _ := metadata.FromOutgoingContext(ctx)
				if md.Get("x-caller")[0] != "own-metadata" {
					t.Error("the join dropped the caller's own metadata")
				}
				return h.write(ctx, connMain, "inner")
			}); err != nil {
				return err
			}
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("err = %v", err)
		}
		if adopted, seen := h.adoptedFor(connMain, "inner"); !seen || !adopted {
			t.Fatalf("the inner write ran OUTSIDE the transaction (adopted=%v)", adopted)
		}
		if got := h.rows(connMain); len(got) != 0 {
			t.Errorf("the inner write survived the rollback: %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Concurrency inside one transaction.

// Parallel nested Runs and emits under one root: every one joins, every parked
// emit runs exactly once after the commit. (No storage writes here — a SQLite
// transaction is one connection; concurrent statements on it are not what is
// under test.)
func TestNested_ParallelJoinsAndEmitsUnderOneRoot(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		const n = 50
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			var wg sync.WaitGroup
			errs := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
						h.bizEmit(ctx, fmt.Sprintf("e%02d", i))
						return nil
					})
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					return err
				}
			}
			if got := h.emittedNow(); len(got) != 0 {
				t.Errorf("announced before commit: %d", len(got))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := h.emittedNow()
		if len(got) != n {
			t.Fatalf("emitted %d, want %d", len(got), n)
		}
		seen := map[string]bool{}
		for _, e := range got {
			if seen[e] {
				t.Errorf("emitted twice: %s", e)
			}
			seen[e] = true
		}
		if h.count("/Begin") != 1 {
			t.Errorf("begin = %d, want 1", h.count("/Begin"))
		}
	})
}

// One of the parallel nested parts fails: the root is rollback-only, nothing
// is announced.
func TestNested_OneParallelFailureMakesTheRootRollBack(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		err := distx.Run(context.Background(), h.client, mainReq(), func(ctx context.Context) error {
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = distx.Run(ctx, h.client, mainReq(), func(ctx context.Context) error {
						h.bizEmit(ctx, fmt.Sprintf("e%02d", i))
						if i == 13 {
							return errBoom
						}
						return nil
					})
				}()
			}
			wg.Wait()
			return nil // every failure swallowed
		})
		if !errors.Is(err, distx.ErrRollbackOnly) {
			t.Fatalf("err = %v", err)
		}
		if got := h.emittedNow(); len(got) != 0 {
			t.Errorf("announced %d events for a rolled-back transaction", len(got))
		}
	})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

var _ = grpc.Header

// The rollback a root runs INSIDE Commit, because a joined part rolled back,
// outlives a done context too — on both transports. TxHandle.Rollback detaching
// its context does not cover this branch: Commit issues the rollback itself.
func TestCommit_RollbackOnlyRollsBackOnADoneContext(t *testing.T) {
	forEachTransport(t, func(t *testing.T, h *harness) {
		ctx, cancel := context.WithCancel(context.Background())
		root, txCtx, err := distx.Begin(ctx, h.client, mainReq())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		joined, jctx, err := distx.Begin(txCtx, h.client, mainReq())
		if err != nil {
			t.Fatalf("nested Begin: %v", err)
		}
		if err := joined.Rollback(jctx); err != nil {
			t.Fatalf("joined Rollback: %v", err)
		}
		cancel()

		err = root.Commit(txCtx)
		if !errors.Is(err, distx.ErrRollbackOnly) {
			t.Fatalf("Commit = %v, want ErrRollbackOnly", err)
		}
		if strings.Contains(err.Error(), "rollback also failed") {
			t.Errorf("the rollback did not go through on a done context: %v", err)
		}
		if h.count("/Rollback") != 1 || h.count("/Commit") != 0 {
			t.Errorf("Rollback calls = %d, Commit calls = %d — want 1 and 0: the transaction is left held until the server times it out",
				h.count("/Rollback"), h.count("/Commit"))
		}
	})
}
