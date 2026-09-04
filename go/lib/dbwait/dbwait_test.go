package dbwait

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type pinger struct {
	failures int32 // refuse this many times, then succeed
	calls    int32
	err      error
}

func (p *pinger) PingContext(context.Context) error {
	n := atomic.AddInt32(&p.calls, 1)
	if n <= atomic.LoadInt32(&p.failures) {
		if p.err != nil {
			return p.err
		}
		return errors.New("connection refused")
	}
	return nil
}

// A database that is already up costs one attempt and no delay. Without
// this the fix would trade a restart loop for a slower boot on every
// project, which is a worse deal than the defect.
func TestPing_ReadyDatabaseIsImmediate(t *testing.T) {
	p := &pinger{}
	start := time.Now()
	if err := Ping(context.Background(), p, "postgres", time.Minute, nil); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Errorf("took %v — a ready database must not pay for the backoff", el)
	}
}

// THE case: a database that refuses and then comes up. Before this, the
// first refusal ended the process — which under compose is a restart, and
// with an ephemeral published port a restart renumbers it.
func TestPing_RetriesUntilTheDatabaseComesUp(t *testing.T) {
	p := &pinger{failures: 3}
	if err := Ping(context.Background(), p, "postgres", 10*time.Second, nil); err != nil {
		t.Fatalf("Ping should have waited out three refusals: %v", err)
	}
	if got := atomic.LoadInt32(&p.calls); got != 4 {
		t.Errorf("calls = %d, want 4 (three refusals + the one that answered)", got)
	}
}

// Failure still fails. A database that never answers is a real fault, and
// the error has to carry the two facts that separate "misconfigured" from
// "still starting": how long it waited and how many times it asked.
func TestPing_GivesUpAndSaysWhat(t *testing.T) {
	p := &pinger{failures: 1 << 30, err: errors.New("no such host")}
	start := time.Now()
	err := Ping(context.Background(), p, "postgres", 300*time.Millisecond, nil)
	if err == nil {
		t.Fatal("an unreachable database must still be an error")
	}
	for _, want := range []string{"postgres", "not accepting connections", "attempts", "no such host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("waited %v, budget was 300ms — the budget is a ceiling, not a hint", el)
	}
}

// A cancelled context stops the wait immediately. A bundle shutting down
// must not sit in a backoff loop, and the error has to say the wait was
// cancelled rather than that the database is broken.
func TestPing_CancelledContextStopsTheWait(t *testing.T) {
	p := &pinger{failures: 1 << 30}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	start := time.Now()
	err := Ping(ctx, p, "postgres", time.Minute, nil)
	if err == nil {
		t.Fatal("a cancelled wait must return an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %q does not wrap context.Canceled", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("waited %v after cancel — the loop ignored ctx", el)
	}
}

// The log fires ONCE on the first refusal and once on recovery, not per
// attempt: the line exists so a slow dependency is visible rather than
// looking like a hang, and a backoff transcript would bury exactly that.
func TestPing_AnnouncesOnceNotPerAttempt(t *testing.T) {
	p := &pinger{failures: 5}
	var lines []string
	if err := Ping(context.Background(), p, "postgres", 10*time.Second,
		func(f string, a ...any) { lines = append(lines, f) }); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if len(lines) != 2 {
		t.Errorf("logged %d line(s) for 5 refusals, want 2 (first refusal + recovery): %v", len(lines), lines)
	}
	// A database that is up logs nothing at all.
	var quiet []string
	_ = Ping(context.Background(), &pinger{}, "postgres", time.Second,
		func(f string, a ...any) { quiet = append(quiet, f) })
	if len(quiet) != 0 {
		t.Errorf("a ready database logged %v", quiet)
	}
}
