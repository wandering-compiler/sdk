package distx

import (
	"context"
	"errors"
	"fmt"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
)

// Run executes fn inside one distributed transaction: begin, then commit when
// fn returns nil and roll back when it does not.
//
// It exists because every project that writes more than once from a business
// method arrives at the same twenty lines, and a consumer counted three places
// in them where a plausible version is wrong:
//
//   - the rollback has to run on a PANIC as well as on an error, or a panicking
//     handler leaves a transaction open until it times out;
//   - the error from Commit must be returned, or a method reports success over
//     a write that did not land;
//   - a business method that refuses AFTER a successful write has to roll back
//     itself. `grpcrollback` covers the storage binary's own handlers; it never
//     sees a decision the business tier makes after the storage call returned
//     nil.
//
// ⚠️ This does not decide WHETHER a method should be transactional. The defect
// the consumer actually hit was not a mis-written helper — it was a batch loop
// that never called [Begin] at all, while its comment claimed atomicity. A
// helper cannot catch that, and it is worth saying so where the helper lives.
//
// fn receives the transaction's context and must use it for every write that
// belongs to the transaction. A write issued on the original ctx opens its own
// transaction and will not be rolled back with this one.
func Run(
	ctx context.Context,
	client distxpb.W17DistributedTransactionClient,
	req *distxpb.BeginRequest,
	fn func(ctx context.Context) error,
) (err error) {
	if client == nil {
		return errors.New("distx.Run: nil transaction client")
	}
	if fn == nil {
		return errors.New("distx.Run: nil function — an empty transaction is a mistake, not a no-op")
	}

	handle, txCtx, err := Begin(ctx, client, req)
	if err != nil {
		return fmt.Errorf("distx.Run: begin: %w", err)
	}

	// Deliberately NOT `defer handle.Rollback()` with a committed flag read at
	// the end: a panic must unwind through a rollback too, and it must keep
	// panicking afterwards. Swallowing it here would turn a crash into a
	// silently failed write, which is strictly worse than the crash.
	// The rollback outlives the caller's context, and the asymmetry with Commit
	// below is the point.
	//
	// The most common way to reach the rollback path is an error, and one of the
	// most common errors is the request context being cancelled — the client
	// hung up, the deadline passed. Issuing Rollback on that same dead context
	// fails before it leaves the process, so exactly when the rollback matters
	// most it does not happen and the transaction is held until the server
	// times it out.
	//
	// COMMIT is deliberately NOT given the same treatment. If the caller is
	// gone, refusing to commit is the safe answer; finishing a write nobody is
	// waiting for is not something a helper should decide on its own.
	rollbackCtx := context.WithoutCancel(ctx)

	done := false
	defer func() {
		if done {
			return
		}
		rerr := handle.Rollback(rollbackCtx)
		if p := recover(); p != nil {
			// The rollback happened; the panic is still the caller's to see.
			panic(p)
		}
		if rerr != nil && err != nil {
			// The function's error is what the caller has to act on, so it
			// stays the error. The rollback failure is real too — a
			// transaction may be left open — so it is named rather than
			// dropped.
			err = fmt.Errorf("%w (rollback also failed: %v)", err, rerr) //nolint:errorlint // rerr is named, not wrapped: errors.Is must answer for fn's error, not the rollback's.
		}
	}()

	if ferr := fn(txCtx); ferr != nil {
		err = ferr
		return err
	}

	// A caller that is gone gets no commit — on EVERY transport. Over the
	// wire the gRPC client refuses a call on a dead context by itself; an
	// in-process conn (a composed binary) dispatches it anyway, so the same
	// cancelled request committed there and rolled back over the wire.
	// Returned BEFORE Commit with done still false: the deferred rollback
	// runs, on its detached context.
	if cerr := ctx.Err(); cerr != nil {
		err = fmt.Errorf("distx.Run: not committing, the caller's context is done: %w", cerr)
		return err
	}
	if cerr := handle.Commit(ctx); cerr != nil {
		// Not wrapped as "the function failed": it did not. Everything fn did
		// is lost, and the message has to say which half went wrong or a
		// reader will look in the wrong place.
		done = true
		return fmt.Errorf("distx.Run: commit: %w", cerr)
	}
	done = true
	return nil
}
