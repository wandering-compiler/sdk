package distx

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/observx"
	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// registerWait bounds how long [RemoteCommits] waits for the coordinator to
// confirm it is watching a transaction. The method is answering its caller
// meanwhile, so the bound is short; a coordinator that cannot confirm in time
// is treated as one that cannot say how the transaction ends.
var registerWait = 10 * time.Second

// RemoteCommits is the [txregistry.CommitHook] of a tier that holds no
// transaction registry of its own — the business tier. It parks a method's
// after-commit work (its events) on a transaction ANOTHER process runs: the
// request arrived over the wire with that process's `w17-tx-id`, so neither a
// local registry nor an in-process scope will ever hear how it ends.
//
// It asks the transaction's coordinator (W17TransactionOutcome.Await, served
// beside W17DistributedTransaction by the storage bundle that minted the id)
// and waits for the answer: the work runs once when the transaction commits
// and never when it does not — what the same method's events do inside one
// process. The wait is registered before the method answers its caller, so the
// caller's commit cannot outrun it.
//
// Delivery is at-most-once, as everywhere after a commit: a process that stops
// while it waits, or a coordinator that cannot say how the transaction ended,
// loses the work, and that is reported through observx.
type RemoteCommits struct {
	client distxpb.W17TransactionOutcomeClient
}

// NewRemoteCommits returns the hook over client — the outcome service on the
// connection the tier's DistributedTransaction client uses. A nil client gives
// a nil hook, which [txregistry.DeferUntilCommit] treats as no hook at all.
func NewRemoteCommits(client distxpb.W17TransactionOutcomeClient) *RemoteCommits {
	if client == nil {
		return nil
	}
	return &RemoteCommits{client: client}
}

var _ txregistry.RemoteCommitHook = (*RemoteCommits)(nil)

// OnCommit implements [txregistry.CommitHook] for a caller with no request
// context (no routing token to echo). Work for a transaction that already
// committed runs at once. Reports whether the work runs or will run.
func (r *RemoteCommits) OnCommit(txID string, fn func()) bool {
	switch r.DeferRemote(context.Background(), txID, fn) {
	case txregistry.EmitNow:
		fn()
		return true
	case txregistry.EmitDeferred:
		return true
	}
	return false
}

// DeferRemote implements [txregistry.RemoteCommitHook]: it registers a wait
// for txID at the coordinator and parks fn on it.
//
//   - [txregistry.EmitDeferred]: the transaction is open; fn runs when it
//     commits and is dropped when it does not.
//   - [txregistry.EmitNow]: it had already committed; the caller runs fn.
//   - [txregistry.EmitDropped]: it had already ended without committing, the
//     coordinator does not know it, or cannot be asked — reported.
//
// ctx is the request's: its `w17-conn-id` (the storage proxy's routing token)
// is echoed so the wait reaches the replica holding the transaction, and its
// cancellation is not inherited — the request ends long before the commit.
func (r *RemoteCommits) DeferRemote(ctx context.Context, txID string, fn func()) txregistry.DeferOutcome {
	if r == nil || fn == nil {
		return txregistry.EmitNow
	}
	md := metadata.MD{}
	if in, ok := metadata.FromIncomingContext(ctx); ok {
		if v := firstNonEmpty(in.Get(ConnIDHeader)); v != "" {
			md.Set(ConnIDHeader, v)
		}
	}
	// A fresh outgoing set: only the routing token. Whatever else the
	// request carried is not this call's to forward.
	wctx, cancel := context.WithCancel(metadata.NewOutgoingContext(context.WithoutCancel(ctx), md))
	reportCtx := context.WithoutCancel(ctx)

	timer := time.AfterFunc(registerWait, cancel)
	stream, err := r.client.Await(wctx, &distxpb.AwaitRequest{TxId: txID})
	var first *distxpb.AwaitResponse
	if err == nil {
		first, err = stream.Recv()
	}
	if !timer.Stop() && err == nil {
		// The bound cut the call after it answered: the stream is gone with
		// it, so the end can no longer be heard.
		err = fmt.Errorf("the coordinator did not confirm the wait within %s", registerWait)
	}
	if err != nil {
		cancel()
		return settleUnwatched(reportCtx, txID, err)
	}
	switch first.GetOutcome() {
	case distxpb.TxOutcome_TX_OUTCOME_COMMITTED:
		cancel()
		return txregistry.EmitNow
	case distxpb.TxOutcome_TX_OUTCOME_NOT_COMMITTED:
		cancel()
		observx.ReportError(reportCtx, fmt.Errorf(
			"distx: dropped the deferred announcement of tx %q: the transaction ended without committing before the emit registered", txID))
		return txregistry.EmitDropped
	}
	// Tracked like the work a local commit releases: a shutdown drain
	// (txregistry.WaitAfterCommit) waits for the answer before the bus and
	// the coordinator connection are closed under it.
	done := txregistry.TrackAfterCommit()
	go func() {
		defer done()
		awaitRemote(reportCtx, txID, stream, cancel, fn)
	}()
	return txregistry.EmitDeferred
}

// awaitRemote reads the rest of an Await stream and runs fn on COMMITTED.
func awaitRemote(ctx context.Context, txID string, stream distxpb.W17TransactionOutcome_AwaitClient, cancel context.CancelFunc, fn func()) {
	defer cancel()
	for {
		msg, err := stream.Recv()
		if err != nil {
			observx.ReportError(ctx, fmt.Errorf(
				"distx: dropped the deferred announcement of tx %q: the coordinator stopped answering before the transaction ended (%w), so whether it committed is unknown", txID, err))
			return
		}
		switch msg.GetOutcome() {
		case distxpb.TxOutcome_TX_OUTCOME_COMMITTED:
			runRemoteHook(ctx, txID, fn)
			return
		case distxpb.TxOutcome_TX_OUTCOME_NOT_COMMITTED:
			// Rolled back: the writes are gone, and so is their announcement.
			return
		}
	}
}

// runRemoteHook runs fn under a recover: the transaction has committed
// whatever fn does, and a panic here has no caller to unwind into.
func runRemoteHook(ctx context.Context, txID string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			observx.ReportError(ctx, fmt.Errorf(
				"distx: after-commit work of committed transaction %s panicked: %v\n%s", txID, p, debug.Stack()))
		}
	}()
	fn()
}

// settleUnwatched decides what to do with work whose wait could not be
// registered.
func settleUnwatched(ctx context.Context, txID string, err error) txregistry.DeferOutcome {
	if status.Code(err) == codes.Unimplemented {
		// A coordinator older than the outcome service: it cannot be asked,
		// and dropping every event in such a mixed deployment would be a
		// regression nobody asked for. Announced now, as before — said.
		observx.ReportError(ctx, fmt.Errorf(
			"distx: announcing inside transaction %q before it commits: its coordinator does not serve W17TransactionOutcome (upgrade the storage bundle), so the event cannot wait for the commit", txID))
		return txregistry.EmitNow
	}
	reason := err.Error()
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		reason = fmt.Sprintf("the coordinator did not confirm the wait within %s", registerWait)
	}
	observx.ReportError(ctx, fmt.Errorf(
		"distx: dropped the deferred announcement of tx %q: its coordinator could not be asked how the transaction ends (%s)", txID, reason))
	return txregistry.EmitDropped
}
