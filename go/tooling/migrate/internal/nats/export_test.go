package nats

import (
	"context"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// SetLockTunablesForTest shrinks the run-lock TTL and heartbeat interval so
// tests exercise expiry and refresh without real-time waits.
func (a *Applier) SetLockTunablesForTest(ttl, beat time.Duration) {
	a.lockTTL = ttl
	a.lockBeat = beat
}

// RunLockBucketForTest names the bucket holding the run lock.
func RunLockBucketForTest() string { return runLockBucket }

// RunLockKeyForTest names the run lock's key in that bucket.
func RunLockKeyForTest() string { return runLockKey }

// RefreshForTest runs one compare-and-set heartbeat refresh on a held lock.
func RefreshForTest(l migrate.RunLock) error {
	return l.(*natsRunLock).refreshOnce(context.Background())
}
