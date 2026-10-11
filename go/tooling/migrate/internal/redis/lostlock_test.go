package redis_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
)

// takenOver acquires a run lock on a1, lets it expire on the server, and has
// a second run take it over — the state of a holder that paused (GC, a
// network partition) past its TTL.
func takenOver(t *testing.T) (*redis.Applier, migrate.RunLock, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	a1 := lockApplier(t, mr, 100*time.Millisecond, time.Hour)
	a2 := lockApplier(t, mr, time.Minute, time.Hour)
	ctx := context.Background()
	l1, err := a1.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a1 acquire: %v", err)
	}
	mr.FastForward(200 * time.Millisecond)
	l2, err := a2.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("a2 takeover: %v", err)
	}
	t.Cleanup(func() { _ = l2.Release(ctx) })
	return a1, l1, mr
}

// A refresh that finds the key gone or owned by someone else has LOST the
// lock. The owner-checked script answers 0 without an error in that case,
// and reading only the error made it a silent success.
func TestRunLock_RefreshOfATakenOverLockReportsItLost(t *testing.T) {
	_, l1, _ := takenOver(t)
	if err := redis.RefreshForTest(l1); err == nil {
		t.Fatal("refreshing a lock another run now holds must fail")
	}
	cl, ok := l1.(migrate.CheckedRunLock)
	if !ok {
		t.Fatal("the Redis run lock must be a CheckedRunLock")
	}
	if err := cl.Check(context.Background()); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check after takeover = %v, want ErrLockLost", err)
	}
}

// Once the lock is lost, the applier stops before its next write — for
// command bodies and data migrations alike — instead of writing next to
// the run that took the lock over.
func TestRunLock_LostLockStopsTheNextWrite(t *testing.T) {
	a1, l1, mr := takenOver(t)
	_ = redis.RefreshForTest(l1) // the heartbeat that discovers the loss
	ctx := liveCtx(t)

	err := a1.Apply(ctx, &applyfetchpb.Migration{Id: "20260101T000000Z", UpSql: "SET plain:k v"})
	if !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("command body after the lock was lost = %v, want ErrLockLost", err)
	}
	if mr.Exists("plain:k") {
		t.Fatal("the command ran after the lock was lost")
	}

	mr.Set("item:1", `{"a":1}`)
	body := `version: 1
encoding: json
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: item:*
    field: b
    value: 2
`
	err = a1.Apply(ctx, &applyfetchpb.Migration{Id: "20260102T000000Z", UpSql: body})
	if !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("data migration after the lock was lost = %v, want ErrLockLost", err)
	}
	if got, _ := mr.Get("item:1"); got != `{"a":1}` {
		t.Fatalf("item:1 = %s; the data migration wrote after the lock was lost", got)
	}
}

// A lease that runs out without a successful refresh counts as lost even
// when nothing has been observed: the store may already have expired it.
func TestRunLock_LeaseRunsOutWithoutARefresh(t *testing.T) {
	mr := miniredis.RunT(t)
	a := lockApplier(t, mr, 50*time.Millisecond, time.Hour) // no heartbeat within the TTL
	l, err := a.AcquireRunLock(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { _ = l.Release(context.Background()) })
	time.Sleep(60 * time.Millisecond)
	if err := l.(migrate.CheckedRunLock).Check(context.Background()); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check past the lease = %v, want ErrLockLost", err)
	}
}

// scanRepeater makes every SCAN page repeat its keys, as a real Redis may
// while the keyspace is rehashed.
type scanRepeater struct{}

func (scanRepeater) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (scanRepeater) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}
func (scanRepeater) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		if sc, ok := cmd.(*goredis.ScanCmd); ok && err == nil {
			page, cursor := sc.Val()
			sc.SetVal(append(append([]string{}, page...), page...), cursor)
		}
		return err
	}
}

// A key SCAN returns twice is transformed once. TRANSFORM_FIELD runs a user
// script that need not be idempotent; applying it again to a repeated key
// corrupts that key with no error.
func TestApply_YAMLTransformAppliesOncePerKeyDespiteScanRepeats(t *testing.T) {
	a, mr := liveApplier(t)
	a.AddHookForTest(scanRepeater{})
	mr.Set("item:1", "a")
	mr.Set("item:2", "b")
	body := `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: item:*
    script_lang: starlark
    script: |
      def transform(value):
          return bytes(str(value) + "!")
`
	if err := a.Apply(liveCtx(t), &applyfetchpb.Migration{Id: "20260101T000000Z", UpSql: body}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for k, want := range map[string]string{"item:1": "a!", "item:2": "b!"} {
		if got, _ := mr.Get(k); got != want {
			t.Errorf("%s = %q, want %q (a repeated SCAN key was transformed more than once)", k, got, want)
		}
	}
}
