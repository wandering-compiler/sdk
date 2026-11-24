//go:build partitionprobe

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// The partition probe: run TestPartitionHolder in one container, disconnect
// that container from the database's network once it prints HELD, then run
// TestPartitionWaiter in another. Both read W17_PARTITION_DSN (a database the
// caller created). The waiter must take the lock once the server has noticed
// the holder is gone — within about a minute with the lock session's
// keepalive, about two hours (the OS default) without it.

func TestPartitionHolder(t *testing.T) {
	dsn := os.Getenv("W17_PARTITION_DSN")
	ctx := context.Background()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.AcquireRunLock(ctx); err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	fmt.Println("HELD")
	time.Sleep(10 * time.Minute)
}

func TestPartitionWaiter(t *testing.T) {
	dsn := os.Getenv("W17_PARTITION_DSN")
	ctx := context.Background()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	start := time.Now()
	for {
		rl, err := a.AcquireRunLock(ctx)
		if err == nil {
			t.Logf("acquired %s after the holder was partitioned", time.Since(start).Round(time.Second))
			_ = rl.Release(ctx)
			return
		}
		if !errors.Is(err, migrate.ErrLockHeld) {
			t.Fatalf("AcquireRunLock: %v", err)
		}
		if time.Since(start) > 3*time.Minute {
			t.Fatalf("the partitioned holder still holds the run lock %s later: the server has not noticed its session is gone", time.Since(start).Round(time.Second))
		}
		time.Sleep(time.Second)
	}
}
