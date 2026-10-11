package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/runlock"
)

var (
	_ migrate.RunLockCapable = (*Applier)(nil)
	_ migrate.CheckedRunLock = (*natsRunLock)(nil)
)

// The run lock lives in a KV bucket of its own, one key per target.
//
// A NATS body is applied line by line with no transaction around it, so two
// runs against one server would both apply the same pending migration — and
// `kv put` / `stream rm` lines are not all idempotent. The lock serialises
// them the way the Redis one does:
//
//   - Acquire is `Create`, which succeeds only when the key holds no value.
//   - The bucket's TTL is the crash-safety net: a holder that dies without
//     releasing stops refreshing, the server ages the entry out, and the
//     next run's Create succeeds. Staleness is judged on the server's clock.
//   - Refresh and release are compare-and-set on the revision this run last
//     wrote, so a run only ever extends or frees a lock it still holds.
//
// Separate from the ledger bucket, because the TTL is a bucket setting: on
// the ledger it would age out the applied history.
const (
	runLockBucket = "w17-migrate-runlock"
	runLockKey    = "runlock"
)

// natsRunLock is a held run lock: the owner, the revision this run last
// wrote, a heartbeat that rewrites it inside the TTL, and the lease that
// says whether it still can.
type natsRunLock struct {
	kv     jetstream.KeyValue
	owner  string
	beater *runlock.Beater
	lease  *runlock.Lease

	mu  sync.Mutex
	rev uint64
}

// AcquireRunLock implements migrate.RunLockCapable. A live lock held by
// another run is migrate.ErrLockHeld (fail fast); a crashed holder's entry
// has aged out of the bucket by then, so acquisition simply succeeds.
func (a *Applier) AcquireRunLock(ctx context.Context) (migrate.RunLock, error) {
	ttl := a.lockTTL
	if ttl <= 0 {
		ttl = runlock.TTL
	}
	beat := a.lockBeat
	if beat <= 0 {
		beat = runlock.Heartbeat
	}
	js, err := a.jsContext()
	if err != nil {
		return nil, err
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      runLockBucket,
		Description: "w17 migrate run lock",
		History:     1,
		TTL:         ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("nats run-lock bucket %s: %w", runLockBucket, err)
	}
	owner := runlock.OwnerID()
	sent := time.Now()
	rev, err := kv.Create(ctx, runLockKey, []byte(owner))
	if err != nil {
		if wrongRevision(err) {
			return nil, migrate.ErrLockHeld
		}
		return nil, fmt.Errorf("nats acquire run-lock: %w", err)
	}
	l := &natsRunLock{kv: kv, owner: owner, rev: rev, lease: runlock.NewLease(ttl, sent)}
	// A heartbeat that fails in silence lets the entry age out mid-run, and
	// a second run can then take the lock and apply next to this one.
	l.beater = runlock.StartBeaterInterval(ctx, beat, l.refreshOnce, func(err error) {
		slog.Warn("run-lock heartbeat refresh failed (lock may expire; concurrent run could double-apply)",
			slog.String("error", err.Error()))
	})
	a.held = l
	return l, nil
}

// refreshOnce rewrites the entry iff it is still the revision this run
// wrote, which restarts its age on the server. Any other revision — or none,
// because the entry aged out — means the lock was lost.
func (l *natsRunLock) refreshOnce(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	sent := time.Now()
	rev, err := l.kv.Update(ctx, runLockKey, []byte(l.owner), l.rev)
	if err != nil {
		if wrongRevision(err) {
			lost := fmt.Errorf("nats run-lock %s/%s no longer holds the revision this run wrote", runLockBucket, runLockKey)
			l.lease.Lose(lost)
			return lost
		}
		// Not proof of loss: the lease runs out on its own if no later
		// refresh gets through in time.
		return fmt.Errorf("nats refresh run-lock: %w", err)
	}
	l.rev = rev
	l.lease.Renewed(sent)
	return nil
}

// Check implements migrate.CheckedRunLock from the lease, without a round
// trip, so the applier can afford it before every write.
func (l *natsRunLock) Check(context.Context) error {
	return l.lease.Check()
}

// Release stops the heartbeat, then deletes the entry iff it is still the
// revision this run wrote. Finding another revision, or none, is reported as
// migrate.ErrLockLost: the entry aged out at some point, so part of the run
// was not protected by it.
func (l *natsRunLock) Release(ctx context.Context) error {
	if l.beater != nil {
		l.beater.Stop()
		l.beater = nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.kv.Delete(ctx, runLockKey, jetstream.LastRevision(l.rev)); err != nil {
		if wrongRevision(err) {
			return fmt.Errorf("nats release run-lock: %w: %s/%s was gone or rewritten by another run at release, so it expired at some point during this run", migrate.ErrLockLost, runLockBucket, runLockKey)
		}
		return fmt.Errorf("nats release run-lock: %w", err)
	}
	return nil
}

// stillHeld is asked before every write: nil when no run lock was taken (an
// applier driven directly, outside a run) or the lock is still ours.
func (a *Applier) stillHeld() error {
	if a.held == nil {
		return nil
	}
	return a.held.lease.Check()
}

// wrongRevision reports the server's refusal of a compare-and-set write: the
// subject's last revision is not the one the write expected.
func wrongRevision(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}
