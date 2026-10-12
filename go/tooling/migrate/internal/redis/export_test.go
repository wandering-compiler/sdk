package redis

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// SetLockTunablesForTest shrinks the run-lock TTL + heartbeat
// interval so tests exercise expiry / refresh without real-time
// waits. Test-only (export_test.go is excluded from the production
// build).
func (a *Applier) SetLockTunablesForTest(ttl, beat time.Duration) {
	a.lockTTL = ttl
	a.lockBeat = beat
}

// RunLockKeyForTest exposes the well-known lock key to external
// tests doing white-box state assertions against miniredis.
func RunLockKeyForTest() string { return runLockKey }

// RefreshForTest runs one owner-checked heartbeat refresh on a held
// lock, so a test can prove the refresh extends the TTL.
func RefreshForTest(l migrate.RunLock) error {
	return l.(*redisRunLock).refreshOnce(context.Background())
}

// AddHookForTest installs a go-redis hook on the applier's client, so a
// test can reshape replies the embedded server would never produce (a
// SCAN page that repeats keys, as a real Redis may during a rehash).
func (a *Applier) AddHookForTest(h goredis.Hook) { a.client.AddHook(h) }
