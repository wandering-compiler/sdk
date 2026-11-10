package distx_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// busyRegistry serves a registry whose finish wait is short, so a Commit
// against a held lease is refused FailedPrecondition quickly.
func busyRegistry(t *testing.T) (distxpb.W17DistributedTransactionClient, *txregistry.Memory) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db}, txregistry.WithFinishWait(50*time.Millisecond))
	srv := grpc.NewServer()
	distxpb.RegisterW17DistributedTransactionServer(srv, distx.NewServer(reg))
	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough://bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return distxpb.NewW17DistributedTransactionClient(conn), reg
}

// Run reports a refused ("busy") Commit as a failure, so it must not leave
// the transaction behind open: it rolls it back. The rollback is decided at
// once (the transaction is doomed) and runs when the holder releases, so
// the locks are not held until the orphan timeout.
func TestRun_ABusyCommitRollsTheTransactionBack(t *testing.T) {
	client, reg := busyRegistry(t)
	var release txregistry.ReleaseFunc
	hookRan := false
	err := distx.Run(context.Background(), client, &distxpb.BeginRequest{ConnectionName: "main"}, func(ctx context.Context) error {
		h, _, err := distx.Begin(ctx, client, &distxpb.BeginRequest{ConnectionName: "main"})
		if err != nil {
			return err
		}
		_, release, err = reg.LookupTx(context.Background(), h.TxID(), "main")
		distx.OnCommit(ctx, func() { hookRan = true })
		return err
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("Run = %v, want the commit's FailedPrecondition", err)
	}
	if strings.Contains(err.Error(), "rollback also failed") {
		t.Errorf("the rollback after a busy commit failed: %v", err)
	}
	if reg.Active() != 1 {
		t.Fatalf("Active() = %d while the lease is held, want 1", reg.Active())
	}
	release()
	if reg.Active() != 0 {
		t.Errorf("Active() = %d after the holder released, want 0 — the transaction was left open for the orphan watcher", reg.Active())
	}
	if hookRan {
		t.Error("work parked on a transaction Run gave up on ran")
	}
}

// The same with the scripted client: the call sequence names the rollback.
func TestRun_ABusyCommitIsFollowedByARollback(t *testing.T) {
	c := &scriptedTx{commitErr: status.Error(codes.FailedPrecondition, "tx_id is in use by another caller")}
	err := distx.Run(context.Background(), c, mainReq(), func(context.Context) error { return nil })
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Run = %v, want FailedPrecondition", err)
	}
	if got := c.seq(); got != "begin,commit,rollback" {
		t.Errorf("calls = %s, want begin,commit,rollback", got)
	}
}

// stallingTx answers Rollback only when its context ends.
type stallingTx struct{ scriptedTx }

func (s *stallingTx) Rollback(ctx context.Context, _ *distxpb.RollbackRequest, _ ...grpc.CallOption) (*distxpb.RollbackResponse, error) {
	<-ctx.Done()
	return nil, status.FromContextError(ctx.Err()).Err()
}

// A rollback detached from a dead request context is still bounded: a
// coordinator that accepted the call and stalls does not hold the business
// handler's goroutine forever.
func TestRollback_TheDetachedRollbackIsBounded(t *testing.T) {
	defer distx.SetRollbackWait(100 * time.Millisecond)()
	for name, finish := range map[string]func(context.Context, *stallingTx) error{
		"TxHandle.Rollback": func(ctx context.Context, c *stallingTx) error {
			h, _, err := distx.Begin(ctx, c, mainReq())
			if err != nil {
				return err
			}
			return h.Rollback(ctx)
		},
		"Run": func(ctx context.Context, c *stallingTx) error {
			return distx.Run(ctx, c, mainReq(), func(context.Context) error { return errors.New("refused") })
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			done := make(chan error, 1)
			go func() { done <- finish(context.WithoutCancel(ctx), &stallingTx{}) }()
			select {
			case err := <-done:
				if err == nil {
					t.Error("a rollback that never answered reported success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the detached rollback is unbounded")
			}
		})
	}
}

// lostReplyTx forwards to a real coordinator but loses the Commit's reply:
// first `before` runs against the server (it may commit or end the
// transaction some other way), then the call fails in transport.
type lostReplyTx struct {
	distxpb.W17DistributedTransactionClient
	before func(ctx context.Context, in *distxpb.CommitRequest) error
}

func (l *lostReplyTx) Commit(ctx context.Context, in *distxpb.CommitRequest, _ ...grpc.CallOption) (*distxpb.CommitResponse, error) {
	if err := l.before(ctx, in); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.Unavailable, "connection reset after the request was sent")
}

// A Commit whose reply was lost after the server committed is settled from
// the coordinator's record of the id, not reported unknown: the settling
// Rollback answers NotFound saying the transaction committed, Commit
// succeeds and the work parked on it runs.
func TestCommit_ALostReplyOfALandedCommitIsSettledAsCommitted(t *testing.T) {
	real, reg := newClientServer(t)
	c := &lostReplyTx{W17DistributedTransactionClient: real, before: func(ctx context.Context, in *distxpb.CommitRequest) error {
		_, err := real.Commit(ctx, in)
		return err
	}}
	ran := false
	err := distx.Run(context.Background(), c, &distxpb.BeginRequest{ConnectionName: "main"}, func(ctx context.Context) error {
		distx.OnCommit(ctx, func() { ran = true })
		return nil
	})
	if err != nil {
		t.Fatalf("Run = %v, want success — the coordinator recorded the commit", err)
	}
	if !ran {
		t.Error("the work parked on a committed transaction did not run")
	}
	if reg.Active() != 0 {
		t.Errorf("Active() = %d, want 0", reg.Active())
	}
}

// The same lost reply over a transaction that ended WITHOUT committing (its
// orphan bound ran out, a participant's failure rolled it back) is an
// ordinary failure, not an unknown outcome.
func TestCommit_ALostReplyOfAnEndedTransactionIsSettledAsNotCommitted(t *testing.T) {
	real, reg := newClientServer(t)
	c := &lostReplyTx{W17DistributedTransactionClient: real, before: func(_ context.Context, in *distxpb.CommitRequest) error {
		return reg.RollbackCaused(context.Background(), in.GetTxId(), "a call inside it failed")
	}}
	ran := false
	err := distx.Run(context.Background(), c, &distxpb.BeginRequest{ConnectionName: "main"}, func(ctx context.Context) error {
		distx.OnCommit(ctx, func() { ran = true })
		return nil
	})
	if err == nil || errors.Is(err, distx.ErrCommitOutcomeUnknown) {
		t.Fatalf("Run = %v, want a settled failure", err)
	}
	if !strings.Contains(err.Error(), "did not commit") {
		t.Errorf("Run = %v, want it to say the transaction did not commit", err)
	}
	if ran {
		t.Error("work parked on a transaction that did not commit ran")
	}
}

// A function that fails after a participant's failure already rolled the
// transaction back (the auto-rollback interceptor) gets its own error back,
// without a "rollback also failed": the coordinator's NotFound says the
// transaction ended without committing, which is what the rollback wanted.
func TestRun_ARollbackOfAnAlreadyRolledBackTransactionIsNotAFailure(t *testing.T) {
	client, reg := newClientServer(t)
	refused := errors.New("the storage call failed")
	err := distx.Run(context.Background(), client, &distxpb.BeginRequest{ConnectionName: "main"}, func(ctx context.Context) error {
		h, _, err := distx.Begin(ctx, client, &distxpb.BeginRequest{ConnectionName: "main"})
		if err != nil {
			return err
		}
		if err := reg.RollbackCaused(context.Background(), h.TxID(), "a call to /x.Y/Z inside it failed"); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Run = %v, want the function's error", err)
	}
	if strings.Contains(err.Error(), "rollback also failed") {
		t.Errorf("Run = %v — a transaction that was already rolled back is reported as a failed rollback", err)
	}
}

// A rollback whose id the coordinator does not remember at all (another
// replica, a restart) is still a failure: the transaction may be open.
func TestRollback_AnIDTheCoordinatorDoesNotKnowIsStillAFailure(t *testing.T) {
	c := &scriptedTx{rollbackErr: status.Error(codes.NotFound, "rollback: txregistry: unknown tx_id")}
	h, _, err := distx.Begin(context.Background(), c, mainReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Rollback(context.Background()); status.Code(err) != codes.NotFound {
		t.Errorf("Rollback = %v, want the NotFound", err)
	}
}
