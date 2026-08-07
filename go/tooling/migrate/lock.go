package migrate

import (
	"context"
	"errors"
)

// ErrLockHeld is returned by AcquireRunLock when another LIVE apply
// run already holds the target's run-lock. The orchestrator turns it
// into a fail-fast abort (a second concurrent apply must not proceed)
// rather than waiting — in the deploy shapes that race (CI pipelines,
// blue/green, N replicas each running migrations on boot) the holder
// will finish the apply, so the loser just exits and the next boot
// sees the work already done.
var ErrLockHeld = errors.New("apply: another apply run holds the target run-lock")

// RunLockCapable is an optional Applier capability: take a
// cross-process advisory lock that serialises concurrent apply runs
// against the same target.
//
// NON-transactional stores (Redis, S3) implement it. Their data
// migrations include TRANSFORM_FIELD — a user Starlark script that
// may be non-idempotent (`price = price * 1.1`) — and two apply runs
// racing the same store would each run it, double-applying and
// silently corrupting data (Q48-datamigrate-1). The op-granularity
// resume cursor only guards a single process resuming after a crash;
// it is a read-check-write with no cross-process atomicity.
//
// Transactional SQL dialects (PG / MySQL / SQLite) do NOT implement
// it: their up_sql runs in a transaction whose wc_migrations INSERT
// has the migration id as a primary key, so a second concurrent run
// fails loudly on the unique violation instead of double-applying.
//
// The orchestrator type-asserts each applier; one that doesn't
// implement RunLockCapable applies without a lock (the transactional
// guarantee covers it).
type RunLockCapable interface {
	// AcquireRunLock takes the target's apply run-lock. It is
	// fail-fast, not blocking: it returns ErrLockHeld immediately
	// when another live run holds the lock, transparently takes over
	// a stale lock (the previous holder crashed without releasing),
	// and on success returns a RunLock whose Release frees it. The
	// returned lock self-refreshes (heartbeat) so a long run doesn't
	// expire under itself.
	AcquireRunLock(ctx context.Context) (RunLock, error)
}

// RunLock is a held apply run-lock. Release frees it and stops the
// heartbeat; it is safe to call exactly once (the orchestrator calls
// it once, in a deferred cleanup).
type RunLock interface {
	Release(ctx context.Context) error
}
