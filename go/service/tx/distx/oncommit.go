package distx

import (
	"context"
	"errors"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/service/tx/txscope"
)

// OnCommit runs fn once the transaction ctx belongs to has COMMITTED, and
// never if it rolls back. Outside a transaction it runs fn at once, on the
// calling goroutine.
//
// It is the hook the generated emit wrapper already uses to hold a method's
// event until the root transaction commits, offered to the author for every
// other side effect that must not outrun the write it describes — an e-mail,
// a call to an external system, a cache purge:
//
//	err := distx.Run(ctx, s.clients.DistributedTransaction(), &distxpb.BeginRequest{}, func(ctx context.Context) error {
//		order, err := s.clients.OrderMutation().CreateOrder(ctx, req)
//		if err != nil {
//			return err
//		}
//		distx.OnCommit(ctx, func() { s.mailer.OrderPlaced(context.WithoutCancel(ctx), order) })
//		return s.clients.StockMutation().Reserve(ctx, …) // a failure here rolls back: no mail
//	})
//
// Pass the context the transaction gave you (the one fn received, or the one
// [Begin] returned): a context outside it runs fn immediately. Inside a nested
// [Run] that JOINED an outer transaction, fn waits for the OUTER commit — a
// joined part never commits on its own.
//
// fn runs on the goroutine that commits, after the commit, in the order the
// hooks were registered, so it must not block for long; hand slow work to a
// goroutine. Its context should be detached from the request
// (`context.WithoutCancel`), which may be cancelled by then. A panic in fn does
// not unwind into the commit — the transaction has landed — and is reported
// through observx. A commit whose outcome is unknown ([ErrCommitOutcomeUnknown])
// runs no hook: announcing a write that may not exist is the worse failure.
//
// The returned outcome says what happened to fn: [txscope.NoScope] or
// [txscope.AlreadyCommitted] (it ran), [txscope.Deferred] (it runs at commit),
// or [txscope.Discarded] (the transaction had already rolled back, so it never
// runs; reported through observx, because registering work on a transaction
// that is over is a bug in the caller).
//
// The Python, TypeScript and Rust business SDKs offer the same hook
// (`on_commit(ctx, fn)`), checked by the sdk's conformance suite.
func OnCommit(ctx context.Context, fn func()) txscope.Outcome {
	if fn == nil {
		return txscope.NoScope
	}
	out := txscope.AfterCommit(ctx, fn)
	switch out {
	case txscope.NoScope, txscope.AlreadyCommitted:
		fn()
	case txscope.Discarded:
		observx.ReportError(ctx, errors.New(
			"distx: OnCommit work dropped: it was registered after its transaction rolled back"))
	}
	return out
}
