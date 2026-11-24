package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// Behind a transaction-mode pooler (PgBouncer pool_mode=transaction) a
// session advisory lock cannot work: each statement may run on another
// server connection. The refusal is right; it has to name the pooler, not
// blame "the session" — which read as a crashed lock holder on every deploy.
func TestRunLock_CheckNamesATransactionPooler_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	b, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()
	rl, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	defer func() { _ = rl.Release(ctx) }()

	err = postgres.RunLockSeenThroughForTest(rl, b).Check(ctx)
	if !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check over another server connection = %v, want ErrLockLost", err)
	}
	if !strings.Contains(err.Error(), "pooler") {
		t.Fatalf("the refusal does not name the connection pooler:\n%v", err)
	}
}
