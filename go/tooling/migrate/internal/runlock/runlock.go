// Package runlock holds the shared pieces of the cross-process
// "apply run-lock" that w17migrate takes before mutating a
// non-transactional store (S3 / Redis). Two concurrent apply runs
// against one target would otherwise double-apply a non-idempotent
// TRANSFORM_FIELD data migration (Q48-datamigrate-1); the lock
// serialises them.
//
// The store-specific acquire / refresh / release lives in each
// applier package (it needs that store's atomic primitive — Redis
// SET NX PX, S3 PutObject If-None-Match). This package only carries
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
	"time"
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

// Beater periodically refreshes a held lock until Stop. Start one
// per acquired lock; Stop it (idempotent) on release. It never fires
// immediately — the lock was just taken — and a refresh error is
// handed to onErr (a lost lock is serious, but the next conditional
// write the applier makes will fail loudly on its own, so the beater
// only reports; it doesn't abort).
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
