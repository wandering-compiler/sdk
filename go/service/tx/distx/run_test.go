package distx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
)

// scriptedTx is a transaction client whose three calls can each be made to
// fail, and which records the order they happened in. Direct rather than over
// bufconn: every assertion here is about WHICH calls Run makes and when, and a
// transport between the two would add a way for the test to be flaky without
// adding anything it asserts.
type scriptedTx struct {
	calls       []string
	beginErr    error
	commitErr   error
	rollbackErr error
}

func (s *scriptedTx) Begin(_ context.Context, _ *distxpb.BeginRequest, _ ...grpc.CallOption) (*distxpb.BeginResponse, error) {
	s.calls = append(s.calls, "begin")
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	return &distxpb.BeginResponse{TxId: "tx-1"}, nil
}

func (s *scriptedTx) Commit(ctx context.Context, _ *distxpb.CommitRequest, _ ...grpc.CallOption) (*distxpb.CommitResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.calls = append(s.calls, "commit")
	if s.commitErr != nil {
		return nil, s.commitErr
	}
	return &distxpb.CommitResponse{}, nil
}

// Rollback HONOURS THE CONTEXT, because a real gRPC client does and the whole
// question here is whether the call can still reach the server. A double that
// records the call and ignores ctx would report success for a rollback that a
// real client refuses to send — proving the call was made, which is not the
// thing that matters.
func (s *scriptedTx) Rollback(ctx context.Context, _ *distxpb.RollbackRequest, _ ...grpc.CallOption) (*distxpb.RollbackResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.calls = append(s.calls, "rollback")
	if s.rollbackErr != nil {
		return nil, s.rollbackErr
	}
	return &distxpb.RollbackResponse{}, nil
}

func (s *scriptedTx) seq() string { return strings.Join(s.calls, ",") }

func TestRunCommitsWhenTheFunctionSucceeds(t *testing.T) {
	c := &scriptedTx{}
	if err := distx.Run(context.Background(), c, &distxpb.BeginRequest{}, func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := c.seq(); got != "begin,commit" {
		t.Errorf("calls = %s, want begin,commit", got)
	}
}

// The function's context must be the TRANSACTION's, not the caller's — a write
// issued on the original ctx opens its own transaction and is not rolled back
// with this one, which is a silent partial write.
func TestRunHandsTheFunctionTheTransactionContext(t *testing.T) {
	c := &scriptedTx{}
	outer := context.Background()
	var inner context.Context
	_ = distx.Run(outer, c, &distxpb.BeginRequest{}, func(ctx context.Context) error {
		inner = ctx
		return nil
	})
	if inner == nil {
		t.Fatal("the function never ran")
	}
	if inner == outer {
		t.Error("the function got the caller's context — writes on it would open their own transaction")
	}
}

func TestRunRollsBackWhenTheFunctionFails(t *testing.T) {
	c := &scriptedTx{}
	want := errors.New("refused after the write")
	err := distx.Run(context.Background(), c, &distxpb.BeginRequest{}, func(context.Context) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want the function's own error", err)
	}
	if got := c.seq(); got != "begin,rollback" {
		t.Errorf("calls = %s, want begin,rollback", got)
	}
}

// A PANIC has to unwind through a rollback and keep panicking. Swallowing it
// would turn a crash into a silently failed write, which is worse than the
// crash; leaving the transaction open makes a panicking handler hold storage
// state until the server times it out.
func TestRunRollsBackOnPanicAndKeepsPanicking(t *testing.T) {
	c := &scriptedTx{}
	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("the panic was swallowed — a crash became a silently failed write")
		}
		if got := c.seq(); got != "begin,rollback" {
			t.Errorf("calls = %s, want begin,rollback", got)
		}
	}()
	_ = distx.Run(context.Background(), c, &distxpb.BeginRequest{}, func(context.Context) error {
		panic("handler exploded")
	})
}

// A failed commit is the case that makes a method LIE: everything the function
// did is gone, and a caller that ignores this error reports success.
func TestRunReturnsTheCommitError(t *testing.T) {
	c := &scriptedTx{commitErr: errors.New("deadlock detected")}
	err := distx.Run(context.Background(), c, &distxpb.BeginRequest{}, func(context.Context) error {
		return nil
	})
	if err == nil {
		t.Fatal("a failed commit reported success — the method would claim a write that is not there")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Errorf("the message does not say which half failed: %v", err)
	}
	// And no rollback after it: the server has already resolved the
	// transaction one way or the other, and a second finishing call would at
	// best fail on an unknown id.
	if got := c.seq(); got != "begin,commit" {
		t.Errorf("calls = %s, want begin,commit with no rollback after", got)
	}
}

// When both fail, the FUNCTION's error is what the caller has to act on — but
// a rollback that did not happen may have left a transaction open, so it is
// named rather than dropped.
func TestRunKeepsTheFunctionErrorAndNamesAFailedRollback(t *testing.T) {
	want := errors.New("business rule refused")
	c := &scriptedTx{rollbackErr: errors.New("connection reset")}
	err := distx.Run(context.Background(), c, &distxpb.BeginRequest{}, func(context.Context) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("the function's error was replaced by the rollback's: %v", err)
	}
	if !strings.Contains(err.Error(), "rollback also failed") {
		t.Errorf("a failed rollback vanished — a transaction may be open: %v", err)
	}
}

func TestRunDoesNotCallTheFunctionWhenBeginFails(t *testing.T) {
	c := &scriptedTx{beginErr: errors.New("no connection")}
	ran := false
	err := distx.Run(context.Background(), c, &distxpb.BeginRequest{}, func(context.Context) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("a failed begin reported success")
	}
	if ran {
		t.Error("the function ran outside a transaction — its writes would each get their own")
	}
	if got := c.seq(); got != "begin" {
		t.Errorf("calls = %s, want begin alone", got)
	}
}

func TestRunRefusesNils(t *testing.T) {
	if err := distx.Run(context.Background(), nil, &distxpb.BeginRequest{}, func(context.Context) error {
		return nil
	}); err == nil {
		t.Error("a nil client was accepted")
	}
	if err := distx.Run(context.Background(), &scriptedTx{}, &distxpb.BeginRequest{}, nil); err == nil {
		t.Error("a nil function was accepted — an empty transaction is a mistake, not a no-op")
	}
}

// The rollback has to survive the caller's context dying, because that is the
// case it exists for.
//
// The most common way to reach the rollback path is an error, and one of the
// most common errors is the request context being cancelled — the client went
// away, the deadline passed. Issuing the Rollback RPC on that same dead context
// fails instantly, so precisely when the rollback matters most it does not
// happen, and the transaction is held until the server times it out.
func TestRunRollsBackEvenWhenTheCallersContextIsDead(t *testing.T) {
	c := &scriptedTx{}
	ctx, cancel := context.WithCancel(context.Background())

	err := distx.Run(ctx, c, &distxpb.BeginRequest{}, func(context.Context) error {
		cancel() // the client hangs up mid-handler
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("a cancelled handler reported success")
	}
	if got := c.seq(); got != "begin,rollback" {
		t.Errorf("calls = %s, want begin,rollback — a rollback issued on the dead "+
			"context never reaches the server, and the transaction is held open", got)
	}
}

// Commit gets the opposite treatment, and the asymmetry is deliberate: if the
// caller is gone, refusing to commit is the safe answer. Finishing a write
// nobody is waiting for is not a decision a helper should make on its own.
func TestRunDoesNotCommitOnBehalfOfAVanishedCaller(t *testing.T) {
	c := &scriptedTx{}
	ctx, cancel := context.WithCancel(context.Background())
	err := distx.Run(ctx, c, &distxpb.BeginRequest{}, func(context.Context) error {
		cancel()
		return nil // the handler succeeded; the caller is gone
	})
	if err == nil {
		t.Error("committed for a caller that had hung up")
	}
}
