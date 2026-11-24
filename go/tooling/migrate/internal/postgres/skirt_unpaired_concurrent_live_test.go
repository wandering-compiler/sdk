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

// unpairedIndexSkirt builds an index CONCURRENTLY with no DROP … IF EXISTS
// before it — the shape a raw post-transaction escape hatch can take.
func unpairedIndexSkirt(unique bool, table string) *applyfetchpb.Migration {
	kw := "INDEX"
	if unique {
		kw = "UNIQUE INDEX"
	}
	return &applyfetchpb.Migration{
		Id: "20260201T000000Z",
		UpSql: `BEGIN;` + ledgerCreate + `
			INSERT INTO w17_migrations VALUES ('2026-02-01T00:00:00Z', 'aa', false);
			COMMIT;`,
		UpPostTx: `CREATE ` + kw + ` CONCURRENTLY books_title ON ` + table + ` (title);
			UPDATE w17_migrations SET post_tx_complete = true WHERE timestamp = '2026-02-01T00:00:00Z';`,
	}
}

// halfAppliedAtTheBuild leaves m's in-transaction half committed and its
// cursor at 0, as a process that died between a CONCURRENTLY build and the
// record of it leaves the ledger; prep runs on the database in between.
func halfAppliedAtTheBuild(t *testing.T, ctx context.Context, a *postgres.Applier, conn *pgx.Conn, m *applyfetchpb.Migration, prep ...string) {
	t.Helper()
	if err := a.Apply(ctx, plainMigration("20260101T000000Z", "2026-01-01T00:00:00Z")); err != nil {
		t.Fatalf("Apply first: %v", err)
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: m.Id, UpSql: m.UpSql}); err != nil {
		t.Fatalf("Apply in-transaction half: %v", err)
	}
	prep = append(prep, `ALTER TABLE w17_migrations ADD COLUMN IF NOT EXISTS post_tx_done INTEGER`,
		`UPDATE w17_migrations SET post_tx_done = 0 WHERE timestamp = '2026-02-01T00:00:00Z'`)
	for _, q := range prep {
		if _, err := conn.Exec(ctx, q); err != nil && q[:6] != "CREATE" {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// qualifiedTable is the schema-qualified table w17 itself emits: an index
// built ON it lands in the table's schema, not on the search_path.
const qualifiedTable = "app.books"

// tablePrep is the setup a table needs before its index is built: none for
// the plain books, a schema and a table of its own for app.books.
func tablePrep(table string) []string {
	if table == "books" {
		return nil
	}
	return []string{`CREATE SCHEMA IF NOT EXISTS app`, `CREATE TABLE app.books (id BIGINT PRIMARY KEY, title TEXT)`}
}

func indexValid(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) bool {
	t.Helper()
	var valid bool
	if err := conn.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1)`, name).Scan(&valid); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return valid
}

// A resume that starts at a CONCURRENTLY build the dead process had already
// finished finds the index built and valid: it records the statement done and
// goes on, instead of failing "already exists" on every deploy.
func TestApplier_ResumeAtAFinishedUnpairedConcurrentBuild_Live(t *testing.T) {
	resumeAtAFinishedBuild(t, "books", "books_title")
}

// The same on a schema-qualified table — the shape w17 emits: the index is
// in the table's schema, which the resume must look in.
func TestApplier_ResumeAtAFinishedUnpairedConcurrentBuild_SchemaQualified_Live(t *testing.T) {
	resumeAtAFinishedBuild(t, qualifiedTable, "app.books_title")
}

func resumeAtAFinishedBuild(t *testing.T, table, index string) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	m := unpairedIndexSkirt(false, table)
	halfAppliedAtTheBuild(t, ctx, a, conn, m, append(tablePrep(table), `CREATE INDEX CONCURRENTLY books_title ON `+table+` (title)`)...)

	if err := a.ApplyPostTx(ctx, m); err != nil {
		t.Fatalf("resume at a build that had finished: %v", err)
	}
	if phase, err := a.MigrationPhase(ctx, m.Id); err != nil || phase != migrate.PhaseComplete {
		t.Fatalf("phase after the resume = %v, %v; want PhaseComplete", phase, err)
	}
	if !indexValid(t, ctx, conn, index) {
		t.Fatal("books_title is not valid after the resume")
	}
}

// A resume that starts at a CONCURRENTLY build which failed part-way finds
// the INVALID index the failure left: it drops it and builds again, instead
// of failing "already exists" over an index that does not work.
func TestApplier_ResumeAtAFailedUnpairedConcurrentBuild_Live(t *testing.T) {
	resumeAtAFailedBuild(t, "books", "books_title")
}

// The same on a schema-qualified table: the INVALID index is in the table's
// schema, and only a DROP of that name removes it.
func TestApplier_ResumeAtAFailedUnpairedConcurrentBuild_SchemaQualified_Live(t *testing.T) {
	resumeAtAFailedBuild(t, qualifiedTable, "app.books_title")
}

func resumeAtAFailedBuild(t *testing.T, table, index string) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	m := unpairedIndexSkirt(true, table)
	// A unique build over duplicates fails and leaves books_title INVALID;
	// the duplicate is then fixed, as an operator would before redeploying.
	halfAppliedAtTheBuild(t, ctx, a, conn, m, append(tablePrep(table),
		`INSERT INTO `+table+` VALUES (1, 'dup'), (2, 'dup')`,
		`CREATE UNIQUE INDEX CONCURRENTLY books_title ON `+table+` (title)`,
		`DELETE FROM `+table+` WHERE id = 2`)...)
	if indexValid(t, ctx, conn, index) {
		t.Fatal("setup: the failed build left a valid index")
	}

	if err := a.ApplyPostTx(ctx, m); err != nil {
		t.Fatalf("resume at a build that had failed: %v", err)
	}
	if phase, err := a.MigrationPhase(ctx, m.Id); err != nil || phase != migrate.PhaseComplete {
		t.Fatalf("phase after the resume = %v, %v; want PhaseComplete", phase, err)
	}
	if !indexValid(t, ctx, conn, index) {
		t.Fatal("books_title is still INVALID after the resume")
	}
}
