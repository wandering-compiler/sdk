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
// the migration set declares (`billing`, created by schema prep) go too, not
// only those in public. Leaving them made the next fresh build find a
// populated store and fail on its first CREATE TABLE.
//
// A schema the set does NOT declare stays, even one owned by the very role
// that wipes: a dev stack connects as a superuser, which may drop anything,
// and another tool's data in the same database is not the project's to
// remove. The session's search_path is the same afterwards.
func TestWipe_DropsDeclaredSchemasOnly_Live(t *testing.T) {
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
		CREATE TABLE public.a (id BIGINT PRIMARY KEY);
		CREATE SCHEMA decoy;
		CREATE TABLE decoy.keep (id BIGINT PRIMARY KEY);`}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := searchPath(ctx, t, a)
	if err := a.Wipe(ctx, []string{"billing", `we"ird`}); err != nil {
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
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema', 'decoy')`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 0 {
		t.Errorf("after Wipe %d tables remain in declared namespaces, want 0", tables)
	}
	var decoy bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('decoy.keep') IS NOT NULL`).Scan(&decoy); err != nil || !decoy {
		t.Errorf("an undeclared schema owned by the same role was dropped (err %v)", err)
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
