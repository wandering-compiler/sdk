// Package runlock holds the shared pieces of the cross-process
// "apply run-lock" that w17migrate takes before mutating a
// non-transactional store (S3 / Redis / NATS). Two concurrent apply runs
// against one target would otherwise double-apply a non-idempotent
// TRANSFORM_FIELD data migration; the lock
// serialises them.
//
// The store-specific acquire / refresh / release lives in each
// applier package (it needs that store's atomic primitive — Redis
// SET NX PX, S3 PutObject If-None-Match, a NATS KV create). This package only carries
// what's common: the owner-token shape, the shared tunables, and a
// heartbeat ticker so a long run keeps its lock alive instead of
// letting it expire underneath itself.
package runlock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Shared tunables. TTL is a lock's lifetime without a refresh;
// Heartbeat re-asserts it well inside that window; StaleAfter is
// how long a store WITHOUT a native TTL (S3) waits before treating
// a never-refreshed lock as abandoned and taking it over. StaleAfter
// is comfortably larger than Heartbeat so a live holder is never
// mistaken for a dead one across modest clock skew.
const (
	TTL        = 5 * time.Minute
	Heartbeat  = 60 * time.Second
	StaleAfter = 3 * time.Minute
)

// OwnerID returns a token identifying THIS process's hold on a lock:
// hostname + pid + a random suffix. The random suffix means two runs
// launched on the same host (same pid reuse across containers, etc.)
// still get distinct owners, so CAS-release / If-Match can never free
// or refresh a lock the current process doesn't actually hold.
func OwnerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	var b [8]byte
	// crypto/rand.Read never returns a short read without an error;
	// on the vanishingly rare error we still emit a usable (if less
	// unique) token rather than fail the whole apply.
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s:%d:norand", host, os.Getpid())
	}
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// Lease answers "is this lock still provably ours?" without a round trip, so
// an applier can ask before every write.
//
// It is measured on THIS process's monotonic clock, from the moment a
// successful acquire or refresh was SENT: the store started the lock's
// lifetime no earlier than that, so the lease ends no later than the lock
// does. A safety margin is taken off on top, so a lease that is about to run
// out counts as gone rather than racing the takeover. Wall-clock time and
// other hosts' clocks play no part.
//
// A refresh that finds another owner marks the lease lost for good: once
// somebody else has held the lock, nothing this process writes afterwards is
// protected, whatever later refreshes report.
type Lease struct {
	mu       sync.Mutex
	length   time.Duration
	deadline time.Time
	lost     error
	now      func() time.Time
}

// NewLease starts a lease of the given length at sentAt, the moment the
// acquiring request was sent.
func NewLease(length time.Duration, sentAt time.Time) *Lease {
	l := &Lease{length: length, now: time.Now}
	l.deadline = l.deadlineFrom(sentAt)
	return l
}

// deadlineFrom keeps a fifth of the length back as margin.
func (l *Lease) deadlineFrom(sentAt time.Time) time.Time {
	return sentAt.Add(l.length - l.length/5)
}

// Renewed extends the lease after a refresh, sent at sentAt, succeeded.
// A lease already lost stays lost.
func (l *Lease) Renewed(sentAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost != nil {
		return
	}
	if d := l.deadlineFrom(sentAt); d.After(l.deadline) {
		l.deadline = d
	}
}

// Lose marks the lease lost with the reason; the first reason wins.
func (l *Lease) Lose(reason error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost == nil {
		l.lost = fmt.Errorf("%w: %v", migrate.ErrLockLost, reason)
	}
}

// Check returns nil while the lease holds and an error wrapping
// migrate.ErrLockLost once it does not.
func (l *Lease) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost != nil {
		return l.lost
	}
	if !l.now().Before(l.deadline) {
		return fmt.Errorf("%w: no successful refresh within the lock's lifetime, so another run may have taken it over", migrate.ErrLockLost)
	}
	return nil
}

// Beater periodically refreshes a held lock until Stop. Start one
// per acquired lock; Stop it (idempotent) on release. It never fires
// immediately — the lock was just taken — and a refresh error is
// handed to onErr. The beater itself does not abort anything: the
// applier asks the lock's Lease before every write, and that is where
// a lost lock stops the run.
type Beater struct {
	stop chan struct{}
	done chan struct{}
}

// StartBeaterInterval starts a heartbeat that refreshes on every tick of the
// given interval (callers pass [Heartbeat] for the production cadence; tests
// pass a short one to avoid waiting a real minute).
func StartBeaterInterval(ctx context.Context, interval time.Duration, refresh func(context.Context) error, onErr func(error)) *Beater {
	b := &Beater{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(b.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := refresh(ctx); err != nil && onErr != nil {
					onErr(err)
				}
			}
		}
	}()
	return b
}

// Stop ends the heartbeat and waits for the goroutine to exit.
// Idempotent — safe to call once; a second call would panic on the
// closed channel, so callers guard with sync.Once or call exactly
// once (the applier RunLock.Release path does the latter).
func (b *Beater) Stop() {
	close(b.stop)
	<-b.done
}
