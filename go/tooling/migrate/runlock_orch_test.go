package migrate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// lockFake is a covFake that also implements RunLockCapable, so the
// orchestrator's acquire-before-apply / release-after-run wiring
// (Q48-datamigrate-1) can be asserted at the run level.
type lockFake struct {
	covFake
	acquireErr      error
	acquires        int32
	releases        int32
	heldDuringApply bool
}

func (f *lockFake) AcquireRunLock(_ context.Context) (RunLock, error) {
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	atomic.AddInt32(&f.acquires, 1)
	return &fakeHeld{f: f}, nil
}

func (f *lockFake) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	// At apply time the lock must be held: acquired once, not yet released.
	if atomic.LoadInt32(&f.acquires) == 1 && atomic.LoadInt32(&f.releases) == 0 {
		f.heldDuringApply = true
	}
	return f.covFake.Apply(ctx, m)
}

type fakeHeld struct{ f *lockFake }

func (h *fakeHeld) Release(_ context.Context) error {
	atomic.AddInt32(&h.f.releases, 1)
	return nil
}

// A RunLockCapable applier must have its lock acquired before any
// migration is applied and released exactly once when the run ends.
func TestRun_AcquiresAndReleasesRunLock(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	dir := seedMigs(t, m)
	tg := tgts(tgt("main", m.GetId(), m.GetContentSha256()))
	fake := &lockFake{}

	if err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (Applier, error) { return fake, nil },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&fake.acquires); got != 1 {
		t.Errorf("acquires = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&fake.releases); got != 1 {
		t.Errorf("releases = %d, want 1", got)
	}
	if !fake.heldDuringApply {
		t.Error("run-lock was not held during Apply")
	}
	if len(fake.applied) != 1 {
		t.Errorf("applied = %v, want [ts-1]", fake.applied)
	}
}

// When the target's run-lock is already held by a live run,
// AcquireRunLock returns ErrLockHeld and the orchestrator must
// fail-fast WITHOUT applying anything (no double-apply).
func TestRun_LockHeldAbortsFailFast(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	dir := seedMigs(t, m)
	tg := tgts(tgt("main", m.GetId(), m.GetContentSha256()))
	fake := &lockFake{acquireErr: ErrLockHeld}

	err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (Applier, error) { return fake, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "another migration run is in progress") {
		t.Fatalf("want fail-fast lock-held abort, got %v", err)
	}
	if len(fake.applied) != 0 {
		t.Errorf("nothing must be applied when the lock is held; got %v", fake.applied)
	}
	if atomic.LoadInt32(&fake.releases) != 0 {
		t.Error("must not release a lock that was never acquired")
	}
}

// A non-ErrLockHeld acquire failure (e.g. the store is unreachable)
// surfaces verbatim, wrapped, and still aborts before applying.
func TestRun_LockAcquireErrorWrapped(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "CREATE TABLE x;")
	dir := seedMigs(t, m)
	tg := tgts(tgt("main", m.GetId(), m.GetContentSha256()))
	fake := &lockFake{acquireErr: errors.New("store unreachable")}

	err := Run(context.Background(), Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (Applier, error) { return fake, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "acquire run-lock") || !strings.Contains(err.Error(), "store unreachable") {
		t.Fatalf("want wrapped acquire error, got %v", err)
	}
	if len(fake.applied) != 0 {
		t.Errorf("nothing must be applied when acquire fails; got %v", fake.applied)
	}
}

// Rollback runs data migrations too, so RunRollback must take the
// run-lock on the same wiring.
func TestRunRollback_AcquiresRunLock(t *testing.T) {
	m := signedMig(t, "ts-1", "main", "a")
	dir := seedMigs(t, m)
	fake := &lockFake{}
	fake.head = "ts-1"

	if err := RunRollback(context.Background(), RollbackConfig{
		Targets: tgts(tgt("main", "", "")), MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(_ string) (Applier, error) { return fake, nil },
	}); err != nil {
		t.Fatalf("RunRollback: %v", err)
	}
	if atomic.LoadInt32(&fake.acquires) != 1 || atomic.LoadInt32(&fake.releases) != 1 {
		t.Errorf("rollback run-lock acquire/release = %d/%d, want 1/1",
			atomic.LoadInt32(&fake.acquires), atomic.LoadInt32(&fake.releases))
	}
	if len(fake.rolled) != 1 {
		t.Errorf("rolled = %v, want [ts-1]", fake.rolled)
	}
}
