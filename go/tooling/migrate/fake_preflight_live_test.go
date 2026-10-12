package migrate_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// fakePGDatabase creates a fresh database on W17_PG_DSN's server and returns
// a DSN for it; dropped on cleanup.
func fakePGDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("W17_PG_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_PG_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_PG_DSN not set — live fake-apply test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	db := fmt.Sprintf("fake_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a, err := pgx.Connect(context.Background(), dsn); err == nil {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			_ = a.Close(context.Background())
		}
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, db)
}

// The adopt preflight and the ledger write run as one unit on a real
// database: a preflight that fails leaves no ledger row behind, and the
// same migration adopts once the check holds.
func TestFake_FailingPreflightLeavesNoLedgerRow_Live(t *testing.T) {
	dsn := fakePGDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m := &applyfetchpb.Migration{
		Id:         "20260101T000000Z",
		Connection: "main",
		UpSql:      "CREATE TABLE books (id BIGINT);",
		DownSql:    "DROP TABLE books;",
		AdoptSql: `CREATE TABLE IF NOT EXISTS w17_migrations (timestamp TIMESTAMPTZ PRIMARY KEY, content_sha256 TEXT NOT NULL, post_tx_complete BOOLEAN NOT NULL DEFAULT true);
			INSERT INTO w17_migrations VALUES ('2026-01-01T00:00:00Z', 'aa', true);`,
		AdoptPreflightSql: `DO $$ BEGIN
			IF to_regclass('public.books') IS NULL THEN RAISE EXCEPTION 'adopt preflight: table books is missing'; END IF;
		END $$;`,
	}
	m.ContentSha256 = migrate.ContentHashWithPreflight(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, m.GetAdoptSql(), m.GetAdoptPreflightSql(), "")
	dir := seedDirUnchained(t, m)
	run := func() error {
		return migrate.Run(ctx, migrate.Config{
			Targets:       []migrate.ConnTarget{{Connection: "main", TargetMigrationID: m.GetId(), TargetContentSha256: m.GetContentSha256()}},
			MigrationsDir: dir,
			ApplierFor:    func(string) (migrate.Applier, error) { return postgres.New(ctx, dsn) },
			Fake:          true,
			Out:           &strings.Builder{},
		})
	}
	probe, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close(context.Background()) }()
	ledgerRows := func() int {
		var exists bool
		if err := probe.QueryRow(ctx, `SELECT to_regclass('w17_migrations') IS NOT NULL`).Scan(&exists); err != nil {
			t.Fatalf("look up the ledger: %v", err)
		}
		if !exists {
			return 0
		}
		var n int
		if err := probe.QueryRow(ctx, `SELECT count(*) FROM w17_migrations`).Scan(&n); err != nil {
			t.Fatalf("count ledger rows: %v", err)
		}
		return n
	}

	if err := run(); err == nil || !strings.Contains(err.Error(), "books is missing") {
		t.Fatalf("--fake with a failing preflight = %v, want the preflight's refusal", err)
	}
	if n := ledgerRows(); n != 0 {
		t.Fatalf("a failing preflight left %d ledger row(s)", n)
	}

	if _, err := probe.Exec(ctx, "CREATE TABLE books (id BIGINT)"); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatalf("--fake once the preflight holds: %v", err)
	}
	if n := ledgerRows(); n != 1 {
		t.Fatalf("adopted migration left %d ledger row(s), want 1", n)
	}
}
