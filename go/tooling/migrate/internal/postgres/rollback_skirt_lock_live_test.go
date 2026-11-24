package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// A rollback undoing a skirt takes the skirt's advisory lock, as the apply of
// the skirt does: an applier resuming that skirt (one without the run lock —
// an older sdk still deployable beside this one) and a rollback undoing it
// must not interleave statement by statement.
func TestRollback_WaitsForTheSkirtLock_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, plainMigration("20260101T000000Z", "2026-01-01T00:00:00Z")); err != nil {
		t.Fatalf("Apply first: %v", err)
	}
	m := skirtMigration("20260201T000000Z", "2026-02-01T00:00:00Z")
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("Apply skirt migration: %v", err)
	}

	other, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = other.Close(context.Background()) }()
	if _, err := other.Exec(ctx, `SELECT pg_advisory_lock(x'77313770737474'::bigint)`); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- a.Rollback(ctx, &applyfetchpb.Migration{Id: m.Id, DownPreTx: m.DownPreTx, DownSql: m.DownSql}) }()
	select {
	case err := <-done:
		t.Fatalf("Rollback returned (%v) while another applier held the skirt lock", err)
	case <-time.After(2 * time.Second):
	}
	if !indexExists(t, ctx, other, "books_title") {
		t.Fatal("the rollback undid the skirt while another applier held its lock")
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock(x'77313770737474'::bigint)`); err != nil {
		t.Fatalf("advisory unlock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Rollback after the lock was released = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Rollback never returned after the lock was released")
	}
	if indexExists(t, ctx, other, "books_title") {
		t.Fatal("rollback left the post-transaction index behind")
	}
}
