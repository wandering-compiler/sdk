package migrate

import (
	"context"
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// hangingReleaseFake holds a run lock whose Release answers only when the
// context it was handed ends — a release round trip over a half-open
// connection at the very end of a run.
type hangingReleaseFake struct {
	covFake
	releaseSawDeadline chan bool
}

func (f *hangingReleaseFake) AcquireRunLock(context.Context) (RunLock, error) {
	return hangingRelease{f}, nil
}

type hangingRelease struct{ f *hangingReleaseFake }

func (h hangingRelease) Release(ctx context.Context) error {
	_, has := ctx.Deadline()
	h.f.releaseSawDeadline <- has
	<-ctx.Done()
	return ctx.Err()
}

// A release that never answers must not hang a run that applied every
// migration: the release gets a bounded context of its own, and its timeout
// is the warning path (the lock goes with the session when it is closed).
func TestRun_HungReleaseDoesNotHangAFullyAppliedRun(t *testing.T) {
	defer func(d time.Duration) { releaseTimeout = d }(releaseTimeout)
	releaseTimeout = 200 * time.Millisecond

	dir, tg := twoMigrations(t)
	fake := &hangingReleaseFake{releaseSawDeadline: make(chan bool, 1)}
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Config{
			Targets: tg, MigrationsDir: dir, Out: &out,
			ApplierFor: func(string) (Applier, error) { return fake, nil },
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil (a release that timed out is a warning)\n%s", err, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run had not returned 5 s after a release with no deadline started; the run hangs after applying everything")
	}
	if !<-fake.releaseSawDeadline {
		t.Errorf("Release was handed a context with no deadline")
	}
	if len(fake.applied) != 2 {
		t.Errorf("applied %v, want both migrations", fake.applied)
	}
	if !strings.Contains(out.String(), "warning: releasing run-lock") {
		t.Errorf("the timed-out release should be reported as a warning:\n%s", out.String())
	}
}

// The release still runs when the run's own context was cancelled: the lock
// has to be given back, and a cancelled context would refuse the round trip
// before it was sent.
func TestRun_ReleaseRunsOnACancelledRunContext(t *testing.T) {
	dir, tg := twoMigrations(t)
	ctx, cancel := context.WithCancel(context.Background())
	var releaseCtxErr error
	fake := &releaseCtxFake{onRelease: func(rctx context.Context) { releaseCtxErr = rctx.Err() }}
	fake.onApply = func() { cancel() }
	_ = Run(ctx, Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(string) (Applier, error) { return fake, nil },
	})
	if !fake.released {
		t.Fatal("the run lock was never released")
	}
	if releaseCtxErr != nil {
		t.Fatalf("Release was handed an already-ended context (%v); the unlock could not be sent", releaseCtxErr)
	}
}

type releaseCtxFake struct {
	covFake
	onApply   func()
	onRelease func(context.Context)
	released  bool
}

func (f *releaseCtxFake) AcquireRunLock(context.Context) (RunLock, error) {
	return releaseCtxLock{f}, nil
}

func (f *releaseCtxFake) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	if f.onApply != nil {
		f.onApply()
	}
	return f.covFake.Apply(ctx, m)
}

type releaseCtxLock struct{ f *releaseCtxFake }

func (l releaseCtxLock) Release(ctx context.Context) error {
	l.f.released = true
	l.f.onRelease(ctx)
	return nil
}
