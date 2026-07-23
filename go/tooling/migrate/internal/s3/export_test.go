package s3

import (
	"context"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// SetLockTunablesForTest injects a fake clock + shrinks the staleness
// window / heartbeat interval so run-lock tests drive the
// stale-takeover and refresh paths deterministically. Test-only.
func (a *Applier) SetLockTunablesForTest(now func() time.Time, stale, beat time.Duration) {
	a.nowFn = now
	a.lockStale = stale
	a.lockBeat = beat
}

// RunLockKeyForTest exposes the lock object key for white-box tests.
func RunLockKeyForTest() string { return runLockKey }

// RefreshForTest runs one heartbeat refresh on a held lock so a test
// can prove the refresh re-stamps heartbeat_at (defeating takeover).
func RefreshForTest(l migrate.RunLock) error {
	return l.(*s3RunLock).refreshOnce(context.Background())
}
