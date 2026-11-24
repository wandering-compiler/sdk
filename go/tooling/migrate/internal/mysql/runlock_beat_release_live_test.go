package mysql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// A heartbeat in flight when the run releases its lock is waited for, so a
// beat that hangs (the holder's network died) must be bounded well under the
// orchestrator's release bound, not by the heartbeat interval. And when that
// beat gives up, the driver closes the lock connection — after the run's last
// write — so the RELEASE_LOCK that cannot be sent is not a lost lock: the run
// finished under it, and the server frees it with the session.
func TestRunLock_AHungBeatAtReleaseIsBoundedAndNotALoss_Live(t *testing.T) {
	defer mysql.SetLockSessionTimingForTest(10*time.Minute, 4*time.Second)()
	defer mysql.SetHeartbeatForTest("DO SLEEP(30)", time.Second)()
	dsn, _ := liveMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	rl, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	time.Sleep(4500 * time.Millisecond) // the first beat is in flight, hanging
	start := time.Now()
	err = rl.Release(ctx)
	took := time.Since(start)
	if took > 2*time.Second {
		t.Errorf("Release waited %v for a hung beat; a beat must be bounded well under the release bound", took)
	}
	if errors.Is(err, migrate.ErrLockLost) {
		t.Errorf("Release after a beat that gave up at release = %v; the run finished under the lock, this is no loss", err)
	}
	if err == nil {
		t.Error("Release over a connection the beat closed answered nil; the failed release must still be said")
	}
}

// A beat that gives up while the run is still going closes the lock
// connection mid-run: that IS a loss, and Release says so.
func TestRunLock_ABeatThatGaveUpMidRunIsALoss_Live(t *testing.T) {
	defer mysql.SetLockSessionTimingForTest(10*time.Minute, 300*time.Millisecond)()
	defer mysql.SetHeartbeatForTest("DO SLEEP(30)", time.Second)()
	dsn, _ := liveMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	rl, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	time.Sleep(3 * time.Second) // a beat hung and gave up; the run goes on
	if err := rl.Release(ctx); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Release after a beat closed the lock connection mid-run = %v; want ErrLockLost", err)
	}
}
