package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// liveDatabase creates a fresh database on the server W17_PG_DSN points at
// and returns a DSN for it; dropped on cleanup.
func liveDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("W17_PG_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_PG_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_PG_DSN not set — live Postgres applier test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	db := fmt.Sprintf("mig_ro_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		a, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			_ = a.Close(context.Background())
		}
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, db)
}

func phaseColumnExists(t *testing.T, ctx context.Context, conn *pgx.Conn) bool {
	t.Helper()
	var has bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'w17_migrations' AND column_name = 'post_tx_complete')`).Scan(&has); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	return has
}

// Reading the applied head is what `migrate status` and every plan do, so it
// must not change the ledger or wait behind a migration in flight. A ledger
// that predates the post-transaction column is read as it is — every row in
// it is complete — and only an apply upgrades it.
func TestAppliedHead_IsReadOnlyOnALegacyLedger_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	setup, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = setup.Close(context.Background()) }()
	if _, err := setup.Exec(ctx, `CREATE TABLE w17_migrations (timestamp TIMESTAMPTZ PRIMARY KEY, content_sha256 TEXT NOT NULL);
		INSERT INTO w17_migrations VALUES ('2026-01-01T00:00:00Z', 'aa')`); err != nil {
		t.Fatalf("legacy ledger: %v", err)
	}

	// A migration in flight: its transaction holds the lock an INSERT into
	// the ledger takes, until it commits.
	inflight, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = inflight.Close(context.Background()) }()
	tx, err := inflight.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE w17_migrations IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	probe, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
	head, err := a.AppliedHead(probe)
	cancelProbe()
	if err != nil {
		t.Fatalf("AppliedHead behind an in-flight migration = %v; a read must not queue behind the ledger's writers", err)
	}
	if head != "20260101T000000Z" {
		t.Errorf("head = %q, want 20260101T000000Z", head)
	}
	probe, cancelProbe = context.WithTimeout(ctx, 5*time.Second)
	phase, err := a.MigrationPhase(probe, "20260101T000000Z")
	cancelProbe()
	if err != nil || phase != migrate.PhaseComplete {
		t.Fatalf("MigrationPhase = %v, %v; want PhaseComplete (a legacy row is complete)", phase, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if phaseColumnExists(t, ctx, setup) {
		t.Fatal("reading the head altered the ledger")
	}

	// The apply path is where the ledger is upgraded.
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "20260201T000000Z", UpSql: `CREATE TABLE t (id BIGINT)`}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !phaseColumnExists(t, ctx, setup) {
		t.Fatal("apply did not add the post-transaction column to a legacy ledger")
	}
}
