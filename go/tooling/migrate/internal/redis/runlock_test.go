package redis_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
)

// lockApplier builds an Applier wired to mr with the given run-lock
// tunables. Two of them on one mr simulate two concurrent apply
// processes racing the same Redis target.
func lockApplier(t *testing.T, mr *miniredis.Miniredis, ttl, beat time.Duration) *redis.Applier {
	t.Helper()
	a, err := redis.New(context.Background(), "redis://"+mr.Addr())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.SetLockTunablesForTest(ttl, beat)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// Q48-datamigrate-1 — the core invariant: while one apply run holds
// the target's run-lock, a second concurrent run is refused
// (migrate.ErrLockHeld) instead of being allowed to double-apply a
// non-idempotent TRANSFORM_FIELD. Releasing frees it for the next.
func TestRunLock_MutualExclusion(t *testing.T) {
	mr := miniredis.RunT(t)
	// Large beat so the heartbeat never interferes with the assertions.
	a1 := lockApplier(t, mr, time.Minute, time.Hour)
	a2 := lockApplier(t, mr, time.Minute, time.Hour)
	ctx := context.Background()

	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := a2.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("second concurrent acquire = %v, want ErrLockHeld", err)
	}

	if err := l1.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if mr.Exists(redis.RunLockKeyForTest()) {
		t.Fatal("release should have removed the lock key")
	}

	l2, err := a2.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := l2.Release(ctx); err != nil {
		t.Fatalf("release l2: %v", err)
	}
}

// A holder that crashes without releasing must not wedge the target
// forever: the PX TTL lapses and the next run takes over cleanly.
// (Redis needs no manual takeover logic — TTL expiry IS the takeover.)
func TestRunLock_StaleTakeoverViaTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	a1 := lockApplier(t, mr, 100*time.Millisecond, time.Hour) // beat huge → no refresh
	a2 := lockApplier(t, mr, time.Minute, time.Hour)
	ctx := context.Background()

	if _, err := a1.AcquireRunLock(ctx); err != nil {
		t.Fatalf("a1 acquire: %v", err)
	}
	// Still held → contended.
	if _, err := a2.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("contended acquire = %v, want ErrLockHeld", err)
	}
	// Simulate the holder vanishing: advance past the TTL.
	mr.FastForward(200 * time.Millisecond)

	l2, err := a2.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("takeover acquire after TTL lapse: %v", err)
	}
	if err := l2.Release(ctx); err != nil {
		t.Fatalf("release l2: %v", err)
	}
}

// After a stale takeover, the ORIGINAL holder's late Release must not
// free the NEW owner's lock — the CAS (owner-checked) release is what
// prevents one run from stomping another's hold.
func TestRunLock_ReleaseIsOwnerChecked(t *testing.T) {
	mr := miniredis.RunT(t)
	a1 := lockApplier(t, mr, 100*time.Millisecond, time.Hour)
	a2 := lockApplier(t, mr, time.Minute, time.Hour)
	ctx := context.Background()

	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a1 acquire: %v", err)
	}
	mr.FastForward(200 * time.Millisecond) // a1's lock expires
	l2, err := a2.AcquireRunLock(ctx)      // a2 takes over
	if err != nil {
		t.Fatalf("a2 takeover: %v", err)
	}

	// a1 (now a zombie) releases late — must be a no-op against a2's lock.
	if err := l1.Release(ctx); err != nil {
		t.Fatalf("zombie release: %v", err)
	}
	if !mr.Exists(redis.RunLockKeyForTest()) {
		t.Fatal("zombie a1.Release wrongly freed a2's lock")
	}
	// a2 still holds → a fresh contender is still refused.
	a3 := lockApplier(t, mr, time.Minute, time.Hour)
	if _, err := a3.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("a3 acquire = %v, want ErrLockHeld (a2 still holds)", err)
	}

	if err := l2.Release(ctx); err != nil {
		t.Fatalf("release l2: %v", err)
	}
}

// The heartbeat refresh must reset the TTL so a long run doesn't
// expire under itself. Drive one refresh explicitly (the beater runs
// the same code) and prove the key survives past its original TTL.
func TestRunLock_RefreshExtendsTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	a1 := lockApplier(t, mr, 200*time.Millisecond, time.Hour) // manual refresh below
	ctx := context.Background()

	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	mr.FastForward(150 * time.Millisecond) // 50ms of TTL left
	if err := redis.RefreshForTest(l1); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	mr.FastForward(150 * time.Millisecond) // would have expired without the refresh

	if !mr.Exists(redis.RunLockKeyForTest()) {
		t.Fatal("refresh did not extend the TTL — lock expired under a live run")
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestAcquireRunLock_DefaultTunables — an Applier with zero TTL/beat
// falls back to the package defaults (runlock.TTL / runlock.Heartbeat)
// rather than acquiring with a non-positive expiry.
func TestAcquireRunLock_DefaultTunables(t *testing.T) {
	mr := miniredis.RunT(t)
	a := lockApplier(t, mr, 0, 0) // zero → defaults kick in
	ctx := context.Background()

	l, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("acquire with default tunables: %v", err)
	}
	if !mr.Exists(redis.RunLockKeyForTest()) {
		t.Fatal("lock key should exist after acquire")
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestAcquireRunLock_SetNXError — a dead Redis target surfaces the SetNX
// error (wrapped), distinct from the ErrLockHeld contention sentinel.
func TestAcquireRunLock_SetNXError(t *testing.T) {
	if _, err := deadApplier(t).AcquireRunLock(boundedCtx(t)); err == nil {
		t.Fatal("acquire against a dead target should error")
	} else if errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("want a connection error, got ErrLockHeld: %v", err)
	}
}

// TestRelease_ScriptError — releasing after the Redis target has gone
// away surfaces the release-script error.
func TestRelease_ScriptError(t *testing.T) {
	mr := miniredis.RunT(t)
	a := lockApplier(t, mr, time.Minute, time.Hour)
	ctx := context.Background()
	l, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	mr.Close() // server gone → release script run fails
	if err := l.Release(ctx); err == nil {
		t.Fatal("release against a closed target should error")
	}
}
