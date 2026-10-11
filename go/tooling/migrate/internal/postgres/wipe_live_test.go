package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// Wipe returns the database to the empty state: tables in the namespaces
// schema prep creates (`billing`) go too, not only those in public. Leaving
// them made the next fresh build find a populated store and fail on its
// first CREATE TABLE. The session's search_path is the same afterwards.
func TestWipe_DropsEveryOwnedSchema_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: `CREATE SCHEMA IF NOT EXISTS "billing";
		CREATE TABLE "billing"."invoices" (id BIGINT PRIMARY KEY);
		CREATE SCHEMA "we""ird";
		CREATE TABLE public.a (id BIGINT PRIMARY KEY);`}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := searchPath(ctx, t, a)
	if err := a.Wipe(ctx); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	if after := searchPath(ctx, t, a); after != before {
		t.Errorf("Wipe changed the session's search_path: %q → %q", before, after)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var tables, schemas int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 0 {
		t.Errorf("after Wipe %d user tables remain, want 0", tables)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_namespace
		WHERE nspname IN ('billing', 'we"ird')`).Scan(&schemas); err != nil {
		t.Fatalf("count schemas: %v", err)
	}
	if schemas != 0 {
		t.Errorf("after Wipe %d schema-prep namespaces remain, want 0", schemas)
	}
	var public bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'public')`).Scan(&public); err != nil || !public {
		t.Errorf("public not recreated (err %v)", err)
	}
}

func searchPath(ctx context.Context, t *testing.T, a *postgres.Applier) string {
	t.Helper()
	sp, err := postgres.SearchPathForTest(ctx, a)
	if err != nil {
		t.Fatalf("read search_path: %v", err)
	}
	return sp
}
