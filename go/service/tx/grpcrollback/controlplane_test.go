package grpcrollback_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/grpcrollback"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

type recordingRoller struct{ rolled []string }

func (r *recordingRoller) Rollback(_ context.Context, txID string) error {
	r.rolled = append(r.rolled, txID)
	return nil
}

// The interceptor must not roll back a transaction because its own COMMIT
// came back retryable.
//
// `take` used to drain the entry on first touch, so a follow-up Rollback
// after a failed Commit landed harmlessly on ErrUnknownTxID — which is what
// this interceptor was built on and what its comments still described. That
// changed when take grew an adoption lease: a Commit that finds a handler
// still holding the lease now returns ErrTxBusy with the entry STILL LIVE,
// which the distx server reports as FailedPrecondition — "a retryable
// not-yet".
//
// The Commit RPC carries `w17-tx-id` itself (TxHandle.attach puts it there,
// and the Begin-derived context carries it regardless), so the interceptor
// saw a failed handler with a tx id and rolled the transaction back. The
// coordinator asked to COMMIT, was told to retry, and the middleware
// converted its intent into a rollback; the retry then gets NotFound, which
// is indistinguishable from "already committed".
//
// Control-plane calls are the transaction's own lifecycle, not work done
// inside it, so they are filtered out.
func TestInterceptor_DoesNotRollBackOnAFailedControlPlaneCall(t *testing.T) {
	roller := &recordingRoller{}
	interceptor := grpcrollback.Interceptor(roller)

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, "tx-1"))
	busy := status.Error(codes.FailedPrecondition, "still has a handler running")

	for _, method := range []string{
		// From the GENERATED constants, not retyped. The interceptor keys
		// on a hand-written prefix of the same service path, so if the
		// service is ever renamed this test must stop agreeing with it —
		// and duplicated literals would rename in lockstep with nothing,
		// leaving both the filter and its pin green while the real method
		// names moved (T3-7 pass #11, C11-5).
		distxpb.W17DistributedTransaction_Commit_FullMethodName,
		distxpb.W17DistributedTransaction_Begin_FullMethodName,
		distxpb.W17DistributedTransaction_Rollback_FullMethodName,
	} {
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
			func(context.Context, any) (any, error) { return nil, busy })
		if !errors.Is(err, busy) {
			t.Errorf("%s: interceptor swallowed the handler error: %v", method, err)
		}
	}
	if len(roller.rolled) != 0 {
		t.Errorf("the interceptor rolled back %v because a control-plane call failed — "+
			"a retryable Commit becomes a destroyed transaction, and the caller's retry "+
			"then gets NotFound", roller.rolled)
	}

	// The permissive half: ordinary work inside the transaction still rolls
	// back on failure. That is the whole point of the interceptor.
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/app.Orders/CreateOrder"},
		func(context.Context, any) (any, error) { return nil, errors.New("write failed") })
	if err == nil {
		t.Fatal("expected the handler error to pass through")
	}
	if len(roller.rolled) != 1 || roller.rolled[0] != "tx-1" {
		t.Errorf("a failed participant RPC must still roll the transaction back; rolled=%v", roller.rolled)
	}
}
