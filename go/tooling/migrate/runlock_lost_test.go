package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// checkedLockFake hands out a CheckedRunLock whose Check starts failing
// once `loseAfter` migrations were applied, and whose Release reports
// what it is told to.
type checkedLockFake struct {
	covFake
	loseAfter  int
	releaseErr error
}

func (f *checkedLockFake) AcquireRunLock(context.Context) (RunLock, error) {
	return &checkedHeld{f: f}, nil
}

type checkedHeld struct{ f *checkedLockFake }

func (h *checkedHeld) Check(context.Context) error {
	if h.f.loseAfter >= 0 && len(h.f.applied) >= h.f.loseAfter {
		return fmt.Errorf("%w: refresh found another owner", ErrLockLost)
	}
	return nil
}

func (h *checkedHeld) Release(context.Context) error { return h.f.releaseErr }

func twoMigrations(t *testing.T) (string, []ConnTarget) {
	t.Helper()
	m1 := signedMig(t, "ts-1", "main", "CREATE TABLE a;")
	m2 := signedMig(t, "ts-2", "main", "CREATE TABLE b;")
	m2.PrevContentSha256 = m1.GetContentSha256()
	m2.ContentSha256 = ContentHash(m2.GetUpSql(), "", "", m2.GetDownSql(), m2.GetPrevContentSha256(), nil, "", "")
	return seedMigs(t, m1, m2), tgts(tgt("main", m2.GetId(), m2.GetContentSha256()))
}

// A run whose lock was lost while one migration ran must not start the
// next: another run may now hold the lock and be applying the same work.
func TestRun_LostRunLockStopsBeforeTheNextMigration(t *testing.T) {
	dir, tg := twoMigrations(t)
	fake := &checkedLockFake{loseAfter: 1}
	err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(string) (Applier, error) { return fake, nil },
	})
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Run = %v, want an error wrapping ErrLockLost", err)
	}
	if len(fake.applied) != 1 {
		t.Fatalf("applied %v; the second migration ran after the lock was lost", fake.applied)
	}
}

// Rollback writes too, so it stops the same way.
func TestRunRollback_LostRunLockStopsBeforeTheNextMigration(t *testing.T) {
	dir, _ := twoMigrations(t)
	fake := &checkedLockFake{loseAfter: -1}
	fake.head = "ts-2"
	ap := &rollbackLosingFake{checkedLockFake: fake}
	err := RunRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir,
		ApplierFor: func(string) (Applier, error) { return ap, nil },
	})
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("RunRollback = %v, want an error wrapping ErrLockLost", err)
	}
	if len(fake.rolled) != 1 {
		t.Fatalf("rolled back %v; the second rollback ran after the lock was lost", fake.rolled)
	}
}

// rollbackLosingFake loses the lock once one rollback ran.
type rollbackLosingFake struct{ *checkedLockFake }

func (f *rollbackLosingFake) AcquireRunLock(context.Context) (RunLock, error) {
	return rollbackHeld{f.checkedLockFake}, nil
}

type rollbackHeld struct{ f *checkedLockFake }

func (h rollbackHeld) Check(context.Context) error {
	if len(h.f.rolled) >= 1 {
		return fmt.Errorf("%w: lease ran out", ErrLockLost)
	}
	return nil
}

func (h rollbackHeld) Release(context.Context) error { return nil }

// Release finding the lock lost fails a run that otherwise succeeded: part
// of it was not protected by the lock, and reporting success over that is
// what the lock exists to prevent.
func TestRun_ReleaseOfALostLockFailsTheRun(t *testing.T) {
	dir, tg := twoMigrations(t)
	fake := &checkedLockFake{loseAfter: -1, releaseErr: fmt.Errorf("release: %w", ErrLockLost)}
	var out strings.Builder
	err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir, Out: &out,
		ApplierFor: func(string) (Applier, error) { return fake, nil },
	})
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Run = %v, want an error wrapping ErrLockLost\n%s", err, out.String())
	}
	if !strings.Contains(err.Error(), "main") {
		t.Errorf("the error should name the connection: %v", err)
	}
}

// Any other release failure (the store went away at the very end) leaves a
// lock that expires by itself; it stays a warning, not a failed run.
func TestRun_OrdinaryReleaseFailureStaysAWarning(t *testing.T) {
	dir, tg := twoMigrations(t)
	fake := &checkedLockFake{loseAfter: -1, releaseErr: errors.New("connection reset")}
	var out strings.Builder
	if err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir, Out: &out,
		ApplierFor: func(string) (Applier, error) { return fake, nil },
	}); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "warning: releasing run-lock") {
		t.Errorf("the release failure should be reported as a warning:\n%s", out.String())
	}
}

// The closing count is what THIS run applied. A migration another run
// applied while this one queued is reported as skipped, not as applied.
func TestRun_ClosingLineCountsOnlyWhatThisRunApplied(t *testing.T) {
	dir, tg := twoMigrations(t)
	fake := &headAdvancingFake{advanceTo: "ts-1"}
	var out strings.Builder
	if err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir, Out: &out,
		ApplierFor: func(string) (Applier, error) { return fake, nil },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.applied != 1 {
		t.Fatalf("applied %d, want 1 (ts-2 only)", fake.applied)
	}
	if !strings.Contains(out.String(), "apply: 1 migration(s) applied, 1 skipped (already applied by a concurrent run)") {
		t.Errorf("closing line does not match the work done:\n%s", out.String())
	}
}
