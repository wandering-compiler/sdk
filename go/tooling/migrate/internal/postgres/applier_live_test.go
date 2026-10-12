package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

const ledgerCreate = `CREATE TABLE IF NOT EXISTS w17_migrations (
	timestamp TIMESTAMPTZ PRIMARY KEY,
	content_sha256 TEXT NOT NULL,
	post_tx_complete BOOLEAN NOT NULL DEFAULT true);`

// skirtMigration is the shape of a migration with a post-transaction half:
// the in-transaction body commits a PENDING ledger row, the skirt builds an
// index CONCURRENTLY on plainMigration's table and flips the row to
// complete.
func skirtMigration(id, ts string) *applyfetchpb.Migration {
	return &applyfetchpb.Migration{
		Id: id,
		UpSql: `BEGIN;` + ledgerCreate + `
			INSERT INTO w17_migrations VALUES ('` + ts + `', 'aa', false);
			COMMIT;`,
		UpPostTx: `DROP INDEX CONCURRENTLY IF EXISTS books_title;
			CREATE INDEX CONCURRENTLY books_title ON books (title);
			UPDATE w17_migrations SET post_tx_complete = true WHERE timestamp = '` + ts + `';`,
		DownPreTx: `DROP INDEX CONCURRENTLY IF EXISTS books_title;`,
		DownSql:   `DELETE FROM w17_migrations WHERE timestamp = '` + ts + `';`,
	}
}

func plainMigration(id, ts string) *applyfetchpb.Migration {
	return &applyfetchpb.Migration{
		Id: id,
		UpSql: `BEGIN;` + ledgerCreate + `
			CREATE TABLE books (id BIGINT PRIMARY KEY, title TEXT);
			INSERT INTO w17_migrations VALUES ('` + ts + `', 'bb', true);
			COMMIT;`,
		DownSql: `BEGIN; DROP TABLE books;
			DELETE FROM w17_migrations WHERE timestamp = '` + ts + `'; COMMIT;`,
	}
}

func indexExists(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) bool {
	t.Helper()
	var has bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&has); err != nil {
		t.Fatalf("read index: %v", err)
	}
	return has
}

// Apply, the applied head, the phase and rollback against a real server: a
// migration whose post-transaction half failed is PENDING and is not the
// head, the resume completes it, and rollback returns the head to the
// migration before it.
func TestApplier_ApplyPhaseHeadRollback_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	probe, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = probe.Close(context.Background()) }()

	if head, err := a.AppliedHead(ctx); err != nil || head != "" {
		t.Fatalf("fresh AppliedHead = %q, %v; want empty", head, err)
	}
	first := plainMigration("20260101T000000Z", "2026-01-01T00:00:00Z")
	if err := a.Apply(ctx, first); err != nil {
		t.Fatalf("Apply first: %v", err)
	}
	if head, err := a.AppliedHead(ctx); err != nil || head != first.Id {
		t.Fatalf("AppliedHead = %q, %v; want %s", head, err, first.Id)
	}

	// The post-transaction half fails: its in-transaction half is committed
	// and pending, and the head stays on the migration before it.
	second := skirtMigration("20260201T000000Z", "2026-02-01T00:00:00Z")
	broken := &applyfetchpb.Migration{Id: second.Id, UpSql: second.UpSql, UpPostTx: `CREATE INDEX CONCURRENTLY books_title ON no_such_table (title);`}
	if err := a.Apply(ctx, broken); err == nil {
		t.Fatal("Apply with a failing post-transaction half succeeded")
	}
	if phase, err := a.MigrationPhase(ctx, second.Id); err != nil || phase != migrate.PhasePending {
		t.Fatalf("MigrationPhase after a failed skirt = %v, %v; want PhasePending", phase, err)
	}
	if head, err := a.AppliedHead(ctx); err != nil || head != first.Id {
		t.Fatalf("AppliedHead with a pending migration = %q, %v; want %s (pending is not applied)", head, err, first.Id)
	}

	// The resume runs only the post-transaction half.
	if err := a.ApplyPostTx(ctx, second); err != nil {
		t.Fatalf("ApplyPostTx: %v", err)
	}
	if phase, err := a.MigrationPhase(ctx, second.Id); err != nil || phase != migrate.PhaseComplete {
		t.Fatalf("MigrationPhase after the resume = %v, %v; want PhaseComplete", phase, err)
	}
	if head, err := a.AppliedHead(ctx); err != nil || head != second.Id {
		t.Fatalf("AppliedHead = %q, %v; want %s", head, err, second.Id)
	}
	if !indexExists(t, ctx, probe, "books_title") {
		t.Fatal("the post-transaction index was not built")
	}

	if err := a.Rollback(ctx, second); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if indexExists(t, ctx, probe, "books_title") {
		t.Fatal("rollback left the post-transaction index behind")
	}
	if !indexExists(t, ctx, probe, "books") {
		t.Fatal("rollback removed the previous migration's table")
	}
	if phase, err := a.MigrationPhase(ctx, second.Id); err != nil || phase != migrate.PhaseFresh {
		t.Fatalf("MigrationPhase after rollback = %v, %v; want PhaseFresh", phase, err)
	}
	if head, err := a.AppliedHead(ctx); err != nil || head != first.Id {
		t.Fatalf("AppliedHead after rollback = %q, %v; want %s", head, err, first.Id)
	}
}

// The post-transaction half runs outside any transaction, so two migrators
// resuming the same migration serialise on an advisory lock and re-check the
// phase under it: the one that waited finds the migration complete and runs
// nothing.
func TestApplier_PostTxWaitsForTheAdvisoryLockAndRechecks_Live(t *testing.T) {
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
	// No leading DROP … IF EXISTS: a waiter that skipped the re-check must
	// fail on the index the other migrator built, not quietly rebuild it.
	m.UpPostTx = `CREATE INDEX CONCURRENTLY books_title ON books (title);
		UPDATE w17_migrations SET post_tx_complete = true WHERE timestamp = '2026-02-01T00:00:00Z';`
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: m.Id, UpSql: m.UpSql}); err != nil {
		t.Fatalf("Apply in-transaction half: %v", err)
	}

	// The other migrator: holds the skirt's lock, builds the index and
	// completes the row while this one waits.
	other, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = other.Close(context.Background()) }()
	var key int64
	if err := other.QueryRow(ctx, `SELECT x'77313770737474'::bigint`).Scan(&key); err != nil {
		t.Fatalf("lock key: %v", err)
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- a.ApplyPostTx(ctx, m) }()
	select {
	case err := <-done:
		t.Fatalf("ApplyPostTx returned (%v) while another migrator held the post-transaction lock", err)
	case <-time.After(2 * time.Second):
	}

	// Without the re-check the waiter would run CREATE INDEX on an index
	// that now exists and fail.
	if _, err := other.Exec(ctx, `CREATE INDEX books_title ON books (title)`); err != nil {
		t.Fatalf("other migrator's index: %v", err)
	}
	if _, err := other.Exec(ctx, `UPDATE w17_migrations SET post_tx_complete = true WHERE timestamp = '2026-02-01T00:00:00Z'`); err != nil {
		t.Fatalf("other migrator's completion: %v", err)
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatalf("advisory unlock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ApplyPostTx after the other migrator completed = %v; want a no-op", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ApplyPostTx never returned after the lock was released")
	}
}
