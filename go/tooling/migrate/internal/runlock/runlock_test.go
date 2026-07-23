package runlock

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerID_DistinctPerCall(t *testing.T) {
	a, b := OwnerID(), OwnerID()
	if a == b {
		t.Fatalf("two OwnerID() calls collided: %q", a)
	}
	// Shape: host:pid:rand — three colon-separated parts.
	if parts := strings.Split(a, ":"); len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		t.Fatalf("OwnerID %q does not match host:pid:rand", a)
	}
}

func TestBeater_FiresThenStops(t *testing.T) {
	var ticks int64
	b := StartBeaterInterval(context.Background(), 10*time.Millisecond, func(context.Context) error {
		atomic.AddInt64(&ticks, 1)
		return nil
	}, nil)

	// Give it room for several ticks.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&ticks) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	b.Stop()
	got := atomic.LoadInt64(&ticks)
	if got < 3 {
		t.Fatalf("beater fired %d times, want >= 3", got)
	}

	// After Stop, no further ticks.
	time.Sleep(50 * time.Millisecond)
	if after := atomic.LoadInt64(&ticks); after != got {
		t.Fatalf("beater kept firing after Stop: %d -> %d", got, after)
	}
}

func TestBeater_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ticks int64
	b := StartBeaterInterval(ctx, 5*time.Millisecond, func(context.Context) error {
		atomic.AddInt64(&ticks, 1)
		return nil
	}, nil)
	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(30 * time.Millisecond)
	stable := atomic.LoadInt64(&ticks)
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt64(&ticks); got != stable {
		t.Fatalf("beater kept firing after ctx cancel: %d -> %d", stable, got)
	}
	b.Stop() // must not hang or panic after a ctx-driven exit
}

func TestBeater_OnErrReceivesError(t *testing.T) {
	var gotErr atomic.Bool
	b := StartBeaterInterval(context.Background(), 5*time.Millisecond,
		func(context.Context) error { return context.DeadlineExceeded },
		func(error) { gotErr.Store(true) })
	deadline := time.Now().Add(time.Second)
	for !gotErr.Load() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	b.Stop()
	if !gotErr.Load() {
		t.Fatal("onErr was never called despite refresh returning an error")
	}
}
