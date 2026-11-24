package mysql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// A live holder keeps its lock however long the run sits between two
// migrations: the lock session's idle limit is short, and the heartbeat keeps
// a live run's session from reaching it.
func TestRunLock_ALiveHolderOutlivesTheSessionIdleLimit_Live(t *testing.T) {
	defer mysql.SetLockSessionTimingForTest(3*time.Second, 500*time.Millisecond)()
	dsn, _ := liveMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	rl, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	time.Sleep(7 * time.Second)
	if err := rl.(migrate.CheckedRunLock).Check(ctx); err != nil {
		t.Fatalf("Check after an idle stretch past the session's idle limit = %v; the live holder lost its lock", err)
	}
	if err := rl.Release(ctx); err != nil {
		t.Fatalf("Release = %v", err)
	}
}

// The heartbeat belongs to the lock, not to the call that took it: an acquire
// context that ends right after the lock is taken (a wait bound, a request's
// deadline) must not silence the heartbeat, or the run loses its lock at the
// session's idle limit while it still believes it holds it.
func TestRunLock_TheHeartbeatOutlivesTheAcquireContext_Live(t *testing.T) {
	defer mysql.SetLockSessionTimingForTest(3*time.Second, 500*time.Millisecond)()
	dsn, _ := liveMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	acquireCtx, endAcquire := context.WithCancel(ctx)
	rl, err := a.AcquireRunLock(acquireCtx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	endAcquire()
	time.Sleep(7 * time.Second)
	if err := rl.(migrate.CheckedRunLock).Check(ctx); err != nil {
		t.Fatalf("Check after the acquire context ended and the session idle limit passed = %v; the heartbeat died with the acquire context", err)
	}
	if err := rl.Release(ctx); err != nil {
		t.Fatalf("Release = %v", err)
	}
}

// A holder that stopped talking to the server — its host lost the network —
// loses the lock within the lock session's idle limit, so the other replicas
// take it instead of waiting out a day-long wait_timeout.
func TestRunLock_ASilentHolderLosesTheLockWithinTheIdleLimit_Live(t *testing.T) {
	defer mysql.SetLockSessionTimingForTest(3*time.Second, 500*time.Millisecond)()
	dsn, _ := liveMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	rl, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	mysql.SilenceForTest(rl)

	b, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()
	start := time.Now()
	for {
		second, err := b.AcquireRunLock(ctx)
		if err == nil {
			t.Logf("taken over %s after the holder went silent", time.Since(start).Round(time.Millisecond))
			_ = second.Release(ctx)
			return
		}
		if !errors.Is(err, migrate.ErrLockHeld) {
			t.Fatalf("AcquireRunLock: %v", err)
		}
		if time.Since(start) > 15*time.Second {
			t.Fatalf("the silent holder still holds the lock %s later: its session is not reaped within the idle limit", time.Since(start).Round(time.Second))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
