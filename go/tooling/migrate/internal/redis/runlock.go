package redis

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/runlock"
)

var (
	_ migrate.RunLockCapable = (*Applier)(nil)
	_ migrate.CheckedRunLock = (*redisRunLock)(nil)
)

// runLockKey is the well-known key holding the apply run-lock. A
// Redis DSN addresses one logical store, so one key locks the whole
// target. SET NX PX makes acquisition atomic; the PX TTL is the
// crash-safety net — a holder that dies without releasing lets the
// key expire, after which the next run acquires cleanly. That makes
// TTL expiry the automatic stale-takeover, so Redis needs no manual
// timestamp / takeover bookkeeping (unlike S3).
const runLockKey = "w17:migrate:runlock"

// refreshScript / releaseScript are owner-checked: this process only
// ever extends or frees a lock it STILL holds, never one a taker
// re-acquired after our TTL lapsed.
var (
	refreshScript = goredis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0`)
	releaseScript = goredis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0`)
)

// redisRunLock is a held run-lock: the owner token plus a heartbeat
// that re-PEXPIREs the key inside its TTL so a long run doesn't
// expire under itself, and the lease that says whether it still can.
type redisRunLock struct {
	client *goredis.Client
	owner  string
	ttlMs  int64
	beater *runlock.Beater
	lease  *runlock.Lease
}

// refreshOnce re-PEXPIREs the lock to its full TTL iff we still own
// it. Shared by the heartbeat goroutine and the tests.
//
// The script's ANSWER is the point: 0 means the key is gone or names
// another owner, so the lock was lost — the TTL lapsed and, possibly,
// another run took it. Reading only the transport error made that a
// silent success, and the run went on writing as if it still held the
// lock.
func (l *redisRunLock) refreshOnce(ctx context.Context) error {
	sent := time.Now()
	n, err := refreshScript.Run(ctx, l.client, []string{runLockKey}, l.owner, l.ttlMs).Int64()
	if err != nil {
		// Not proof of loss: the lease runs out on its own if no later
		// refresh gets through in time.
		return fmt.Errorf("redis refresh run-lock: %w", err)
	}
	if n != 1 {
		err := fmt.Errorf("redis run-lock %s no longer names this run as its owner", runLockKey)
		l.lease.Lose(err)
		return err
	}
	l.lease.Renewed(sent)
	return nil
}

// Check implements migrate.CheckedRunLock from the lease, without a round
// trip, so the applier can afford it before every write.
func (l *redisRunLock) Check(context.Context) error {
	return l.lease.Check()
}

// AcquireRunLock implements migrate.RunLockCapable. SET NX PX: success
// means we hold it; failure means another LIVE run holds it
// (migrate.ErrLockHeld → fail-fast). A crashed prior holder's key has
// already expired by TTL, so acquisition just succeeds.
func (a *Applier) AcquireRunLock(ctx context.Context) (migrate.RunLock, error) {
	ttl := a.lockTTL
	if ttl <= 0 {
		ttl = runlock.TTL
	}
	beat := a.lockBeat
	if beat <= 0 {
		beat = runlock.Heartbeat
	}
	owner := runlock.OwnerID()

	sent := time.Now()
	ok, err := a.client.SetNX(ctx, runLockKey, owner, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("redis acquire run-lock: %w", err)
	}
	if !ok {
		return nil, migrate.ErrLockHeld
	}

	l := &redisRunLock{client: a.client, owner: owner, ttlMs: ttl.Milliseconds(), lease: runlock.NewLease(ttl, sent)}
	// A swallowed heartbeat failure lets the TTL lapse mid-run, after
	// which a second run can acquire the key and double-apply a
	// non-idempotent migration — exactly what the lock exists to stop.
	// Surface it as a WARNING (the beater has no return path).
	l.beater = runlock.StartBeaterInterval(ctx, beat, l.refreshOnce, func(err error) {
		slog.Warn("run-lock heartbeat refresh failed (lock may expire; concurrent run could double-apply)",
			slog.String("error", err.Error()))
	})
	// Every write this applier makes from now on asks the lease first.
	a.held = l
	return l, nil
}

// stillHeld is asked before every write: nil when no run lock was taken
// (an applier driven directly, outside a run) or the lock is still ours.
func (a *Applier) stillHeld() error {
	if a.held == nil {
		return nil
	}
	return a.held.lease.Check()
}

// Release stops the heartbeat, then frees the lock iff we still own
// it. Releasing an expired / taken-over key deletes nothing (the owner
// check), and is REPORTED as migrate.ErrLockLost: the key only names
// this run if it never expired, so finding it gone or someone else's
// means part of the run was not protected by it.
func (l *redisRunLock) Release(ctx context.Context) error {
	if l.beater != nil {
		l.beater.Stop()
		l.beater = nil
	}
	n, err := releaseScript.Run(ctx, l.client, []string{runLockKey}, l.owner).Int64()
	if err != nil {
		return fmt.Errorf("redis release run-lock: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("redis release run-lock: %w: %s was gone or held by another run at release, so it expired at some point during this run", migrate.ErrLockLost, runLockKey)
	}
	return nil
}
