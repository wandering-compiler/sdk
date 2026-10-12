package runlock

import (
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// fakeNow is a settable clock for the lease.
type fakeNow struct{ t time.Time }

func (f *fakeNow) now() time.Time { return f.t }

func TestLease_HoldsUntilItsMarginThenRunsOut(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1_000, 0)}
	l := NewLease(100*time.Second, clk.t)
	l.now = clk.now

	clk.t = clk.t.Add(79 * time.Second)
	if err := l.Check(); err != nil {
		t.Fatalf("Check inside the lease = %v", err)
	}
	// A fifth of the length is kept back: at 80 s of 100 the lease is over,
	// before the store could expire the lock.
	clk.t = clk.t.Add(1 * time.Second)
	if err := l.Check(); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check at the margin = %v, want ErrLockLost", err)
	}
}

func TestLease_RenewalExtendsFromWhenTheRefreshWasSent(t *testing.T) {
	start := time.Unix(1_000, 0)
	clk := &fakeNow{t: start}
	l := NewLease(100*time.Second, start)
	l.now = clk.now

	l.Renewed(start.Add(60 * time.Second))
	clk.t = start.Add(130 * time.Second) // 70 s after the refresh was sent
	if err := l.Check(); err != nil {
		t.Fatalf("Check after a renewal = %v", err)
	}
	clk.t = start.Add(140 * time.Second)
	if err := l.Check(); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check past the renewed lease = %v, want ErrLockLost", err)
	}
}

func TestLease_LostStaysLost(t *testing.T) {
	start := time.Unix(1_000, 0)
	l := NewLease(time.Hour, start)
	l.now = func() time.Time { return start }

	l.Lose(errors.New("another owner"))
	l.Renewed(start) // a later refresh cannot undo a loss
	if err := l.Check(); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check after Lose = %v, want ErrLockLost", err)
	}
}
