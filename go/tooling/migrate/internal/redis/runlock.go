package redis

import (
	"context"
	"fmt"
	"log/slog"

	goredis "github.com/redis/go-redis/v9"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/runlock"
)

var _ migrate.RunLockCapable = (*Applier)(nil)

// runLockKey is the well-known key holding the apply run-lock. A
// Redis DSN addresses one logical store, so one key locks the whole
// target. SET NX PX makes acquisition atomic; the PX TTL is the
// crash-safety net — a holder that dies without releasing lets the
// key expire, after which the next run acquires cleanly. That makes
// TTL expiry the automatic stale-takeover, so Redis needs no manual
// timestamp / takeover bookkeeping (unlike S3).
const runLockKey = "wc:migrate:runlock"

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
// expire under itself.
type redisRunLock struct {
	client *goredis.Client
	owner  string
	ttlMs  int64
	beater *runlock.Beater
}

// refreshOnce re-PEXPIREs the lock to its full TTL iff we still own
// it. Shared by the heartbeat goroutine and the tests.
func (l *redisRunLock) refreshOnce(ctx context.Context) error {
	return refreshScript.Run(ctx, l.client, []string{runLockKey}, l.owner, l.ttlMs).Err()
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

	ok, err := a.client.SetNX(ctx, runLockKey, owner, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("redis acquire run-lock: %w", err)
	}
	if !ok {
		return nil, migrate.ErrLockHeld
	}

	l := &redisRunLock{client: a.client, owner: owner, ttlMs: ttl.Milliseconds()}
	// A swallowed heartbeat failure lets the TTL lapse mid-run, after
	// which a second run can acquire the key and double-apply a
	// non-idempotent migration — exactly what the lock exists to stop.
	// Surface it as a WARNING (the beater has no return path).
	l.beater = runlock.StartBeaterInterval(ctx, beat, l.refreshOnce, func(err error) {
		slog.Warn("run-lock heartbeat refresh failed (lock may expire; concurrent run could double-apply)",
			slog.String("error", err.Error()))
	})
	return l, nil
}

// Release stops the heartbeat, then frees the lock iff we still own
// it. Idempotent at the Redis layer (releasing an expired / taken-
// over key is a no-op via the owner check).
func (l *redisRunLock) Release(ctx context.Context) error {
	if l.beater != nil {
		l.beater.Stop()
		l.beater = nil
	}
	if err := releaseScript.Run(ctx, l.client, []string{runLockKey}, l.owner).Err(); err != nil {
		return fmt.Errorf("redis release run-lock: %w", err)
	}
	return nil
}
