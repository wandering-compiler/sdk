package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// renameOntoFreedSlot is the skirt the migrator writes when a table takes the
// name of a table it drops in the same migration, next to a concurrent index
// (T1-1 pass #50, D50-2b): drop the old table, rename the other onto its name,
// then tighten a column of the renamed table — the statement that fails on a
// NULL row, inviting the documented "fix the row and deploy again".
func renameOntoFreedSlot(id, ts string) *applyfetchpb.Migration {
	return &applyfetchpb.Migration{
		Id: id,
		UpSql: `BEGIN;` + ledgerCreate + `
			INSERT INTO w17_migrations (timestamp, content_sha256, post_tx_complete) VALUES ('` + ts + `', 'cc', false);
			COMMIT;`,
		UpPostTx: `DROP INDEX CONCURRENTLY IF EXISTS old_t_x_idx;
DROP TABLE IF EXISTS "old_t";
ALTER TABLE "t2" RENAME TO "old_t";
ALTER TABLE "old_t" ALTER COLUMN "v" SET NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS old_t_v_idx;
CREATE INDEX CONCURRENTLY old_t_v_idx ON "old_t" ("v");
UPDATE w17_migrations SET post_tx_complete = true WHERE timestamp = '` + ts + `';`,
	}
}

func skirtRows(t *testing.T, ctx context.Context, conn *pgx.Conn, q string) string {
	t.Helper()
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return "ERROR: " + err.Error()
		}
		var cells []string
		for _, v := range vals {
			if v == nil {
				cells = append(cells, "null")
				continue
			}
			cells = append(cells, fmt.Sprint(v))
		}
		out = append(out, strings.Join(cells, "|"))
	}
	if err := rows.Err(); err != nil {
		return "ERROR: " + err.Error()
	}
	return strings.Join(out, ";")
}

// P50-18 (R1: D50-1 + D50-2 + D50-2b) — a skirt that stopped part-way
// resumes AFTER the statements it already ran. It used to run again from the
// first: here the leading `DROP TABLE IF EXISTS "old_t"` dropped the renamed
// table with its three rows, and the resume failed on `"t2" does not exist`.
func TestApplier_SkirtResumesWhereItStopped_Live(t *testing.T) {
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
	for _, q := range []string{
		ledgerCreate,
		`INSERT INTO w17_migrations (timestamp, content_sha256) VALUES ('2026-01-01T00:00:00Z', 'aa')`,
		`CREATE TABLE old_t (id BIGINT PRIMARY KEY, x BIGINT)`, `CREATE INDEX old_t_x_idx ON old_t (x)`,
		`CREATE TABLE t2 (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO t2 VALUES (1, 10), (2, 20), (3, NULL)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	m := renameOntoFreedSlot("20260201T000000Z", "2026-02-01T00:00:00Z")

	err = a.Apply(ctx, m)
	if err == nil {
		t.Fatal("the skirt's SET NOT NULL applied over a NULL")
	}
	for _, want := range []string{"statement 4 of 7", "half-applied", "first 3 statement(s)", "resumes at statement 4", `migrate rollback --to "20260101T000000Z"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not say %q:\n%v", want, err)
		}
	}
	if got := skirtRows(t, ctx, conn, `SELECT id, v FROM old_t ORDER BY id`); got != "1|10;2|20;3|null" {
		t.Fatalf("after the failure old_t = %q, want the renamed rows", got)
	}

	// The documented remedy: fix the row, deploy again.
	if _, err := conn.Exec(ctx, `UPDATE old_t SET v = 30 WHERE v IS NULL`); err != nil {
		t.Fatal(err)
	}
	if phase, err := a.MigrationPhase(ctx, m.Id); err != nil || phase != migrate.PhasePending {
		t.Fatalf("phase = %v, %v; want PhasePending", phase, err)
	}
	if err := a.ApplyPostTx(ctx, m); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := skirtRows(t, ctx, conn, `SELECT id, v FROM old_t ORDER BY id`); got != "1|10;2|20;3|30" {
		t.Fatalf("after the resume old_t = %q, want 1|10;2|20;3|30 — the resume re-ran the drop of the freed name", got)
	}
	if phase, err := a.MigrationPhase(ctx, m.Id); err != nil || phase != migrate.PhaseComplete {
		t.Fatalf("phase after the resume = %v, %v; want PhaseComplete", phase, err)
	}
	if !indexExists(t, ctx, conn, "old_t_v_idx") {
		t.Fatal("the concurrent index after the resume point was not built")
	}
}

// A ledger created before the cursor column gains it on the first skirt, and
// a concurrent build whose cursor was not recorded (the process died between
// the build and the record) is re-run with the DROP … IF EXISTS before it —
// never frozen INVALID, never "already exists".
func TestApplier_SkirtCursorBootstrapsAndRerunsAConcurrentPair_Live(t *testing.T) {
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
	if err := a.Apply(ctx, plainMigration("20260101T000000Z", "2026-01-01T00:00:00Z")); err != nil {
		t.Fatalf("Apply first: %v", err)
	}
	// Minted with the witness (T1-1 pass #51, Q51-1): the in-tx half alone
	// leaves a NULL cursor, which under the witness means nothing ran.
	m := skirtMigration("20260201T000000Z", "2026-02-01T00:00:00Z")
	m.UpPostTx = p51Witness + m.UpPostTx
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: m.Id, UpSql: m.UpSql}); err != nil {
		t.Fatalf("in-transaction half: %v", err)
	}
	// The build ran, the record of it did not: the cursor still says 0.
	if _, err := conn.Exec(ctx, `CREATE INDEX books_title ON books (title)`); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyPostTx(ctx, m); err != nil {
		t.Fatalf("resume over a built, unrecorded concurrent index: %v", err)
	}
	var done int
	if err := conn.QueryRow(ctx, `SELECT post_tx_done FROM w17_migrations WHERE timestamp = '2026-02-01T00:00:00Z'`).Scan(&done); err != nil || done != 4 {
		t.Fatalf("post_tx_done = %d, %v; want 4 (the witness and three statements)", done, err)
	}
}
