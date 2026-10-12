package nats_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
)

// secondApplier opens another applier against the same server, as a second
// run would.
func secondApplier(t *testing.T, url string, ttl, beat time.Duration) *nats.Applier {
	t.Helper()
	a, err := nats.New(context.Background(), url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.SetLockTunablesForTest(ttl, beat)
	return a
}

// The NATS applier takes a run lock, so the orchestrator serialises two runs
// against one server instead of letting both apply the same body.
func TestNATSRunLock_IsTakenByTheOrchestrator(t *testing.T) {
	var a any = &nats.Applier{}
	if _, ok := a.(migrate.RunLockCapable); !ok {
		t.Fatal("the NATS applier does not take a run lock")
	}
}

// While one run holds the lock a second is refused at once; after release
// it acquires.
func TestNATSRunLock_MutualExclusion(t *testing.T) {
	a1, url := liveApplier(t)
	a2 := secondApplier(t, url, 0, 0)
	ctx := liveCtx(t)

	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a1 acquire: %v", err)
	}
	if _, err := a2.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("a2 acquire while a1 holds = %v, want ErrLockHeld", err)
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatalf("a1 release: %v", err)
	}
	l2, err := a2.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a2 acquire after release: %v", err)
	}
	if err := l2.Release(ctx); err != nil {
		t.Fatalf("a2 release: %v", err)
	}
}

// acquireWithin retries until the lock is free or the deadline passes.
func acquireWithin(t *testing.T, a *nats.Applier, d time.Duration) migrate.RunLock {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		l, err := a.AcquireRunLock(context.Background())
		if err == nil {
			return l
		}
		if !errors.Is(err, migrate.ErrLockHeld) || time.Now().After(deadline) {
			t.Fatalf("acquire: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A holder that stops refreshing (it crashed, or stalled past the TTL) has
// its entry aged out by the server, and the next run takes over. The stalled
// holder then finds out: its refresh fails, its lease is lost for good, its
// next write is refused before it runs, and its release reports ErrLockLost
// without freeing the new holder's lock.
func TestNATSRunLock_StaleTakeoverAndLostHolder(t *testing.T) {
	const ttl = time.Second
	a1, url := liveApplier(t)
	a1.SetLockTunablesForTest(ttl, time.Hour) // never refreshes on its own
	a2 := secondApplier(t, url, ttl, 200*time.Millisecond)
	a3 := secondApplier(t, url, ttl, time.Hour)
	ctx := liveCtx(t)

	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a1 acquire: %v", err)
	}
	l2 := acquireWithin(t, a2, 5*time.Second)
	t.Cleanup(func() { _ = l2.Release(context.Background()) })

	if err := nats.RefreshForTest(l1); err == nil {
		t.Fatal("refreshing a lock another run now holds succeeded")
	}
	if err := l1.(migrate.CheckedRunLock).Check(ctx); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check after takeover = %v, want ErrLockLost", err)
	}

	err = a1.Apply(ctx, &applyfetchpb.Migration{UpSql: "nats kv add stalled"})
	if !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("a write after the lock was lost = %v, want ErrLockLost", err)
	}
	js := directJS(t, url)
	if _, err := js.KeyValue(ctx, "stalled"); !errors.Is(err, jetstream.ErrBucketNotFound) {
		t.Fatalf("the stalled holder's line ran after its lock was lost (bucket lookup: %v)", err)
	}

	if err := l1.Release(ctx); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("release of a taken-over lock = %v, want ErrLockLost", err)
	}
	if _, err := a3.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("after the stale holder's release a third run acquired (%v): it freed the new holder's lock", err)
	}
}

// A holder that keeps refreshing is never taken over, however long it runs.
func TestNATSRunLock_RefreshPreventsTakeover(t *testing.T) {
	const ttl = time.Second
	a1, url := liveApplier(t)
	a1.SetLockTunablesForTest(ttl, 200*time.Millisecond)
	a2 := secondApplier(t, url, ttl, time.Hour)
	ctx := liveCtx(t)

	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a1 acquire: %v", err)
	}
	time.Sleep(3 * ttl)
	if _, err := a2.AcquireRunLock(ctx); !errors.Is(err, migrate.ErrLockHeld) {
		t.Fatalf("a refreshed lock was taken over: %v", err)
	}
	if err := l1.(migrate.CheckedRunLock).Check(ctx); err != nil {
		t.Fatalf("a refreshed lock reports itself lost: %v", err)
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// Two runs racing for one server: exactly one gets the lock.
func TestNATSRunLock_ConcurrentAcquireHasOneWinner(t *testing.T) {
	_, url := liveApplier(t)
	const n = 8
	type result struct {
		l   migrate.RunLock
		err error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		a := secondApplier(t, url, 0, 0)
		go func() {
			l, err := a.AcquireRunLock(context.Background())
			results <- result{l, err}
		}()
	}
	winners := 0
	for i := 0; i < n; i++ {
		r := <-results
		switch {
		case r.err == nil:
			winners++
			defer func() { _ = r.l.Release(context.Background()) }()
		case !errors.Is(r.err, migrate.ErrLockHeld):
			t.Errorf("acquire: %v", r.err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d runs hold the lock at once, want 1", winners)
	}
}

// A snapshot carries the store, not a run's lock: restored without its TTL
// the lock would never age out and would refuse every later run.
func TestNATSRunLock_IsNotSnapshotted(t *testing.T) {
	ctx := context.Background()
	srcURL := bootNATS(t)
	holder := secondApplier(t, srcURL, 0, 0)
	l, err := holder.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = l.Release(ctx) }()

	src, err := nats.NewSnapshotter(srcURL)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.Dump(ctx, &buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	dstURL := bootNATS(t)
	dst, err := nats.NewSnapshotter(dstURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Restore(ctx, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := jsFor(t, dstURL).KeyValue(ctx, nats.RunLockBucketForTest()); !errors.Is(err, jetstream.ErrBucketNotFound) {
		t.Fatalf("the run lock travelled with the snapshot (lookup: %v)", err)
	}
	if _, err := secondApplier(t, dstURL, 0, 0).AcquireRunLock(ctx); err != nil {
		t.Fatalf("a run against the restored store cannot take the lock: %v", err)
	}
}
