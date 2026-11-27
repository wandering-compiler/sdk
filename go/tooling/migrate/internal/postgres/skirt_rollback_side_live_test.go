package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// The rollback side of the skirt: per-statement undo inside a half-run
// operation, the settle of an interrupted CONCURRENTLY inverse at the resume
// point, ledger readers that do not call a migration being undone applied, and
// the served (signed) shape of a skirt reaching the pre-commit cursor gate.

const prevMigrationID = "20260101T000000Z"

// pendingInsert is the in-tx half applied.Wrap gives a skirt migration: the
// pending ledger row.
const pendingInsert = "BEGIN;\n\nINSERT INTO \"w17_migrations\" (\"timestamp\", \"content_sha256\", \"post_tx_complete\") VALUES ('" + witnessMigrationTS + "', 'cc', false);\n\nCOMMIT;\n"

const completeUpdate = `UPDATE "w17_migrations" SET "post_tx_complete" = true WHERE "timestamp" = '` + witnessMigrationTS + `';`

const ledgerErase = `-- w17:skirt-op always
DELETE FROM "w17_migrations" WHERE "timestamp" = '` + witnessMigrationTS + `';`

func seedSQL(t *testing.T, ctx context.Context, conn *pgx.Conn, qs ...string) {
	t.Helper()
	for _, q := range append([]string{ledgerCreate,
		`INSERT INTO w17_migrations (timestamp, content_sha256) VALUES ('2026-01-01T00:00:00Z', 'aa')`}, qs...) {
		if _, err := conn.Exec(ctx, q, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// dropTableSkirt is a DropTable riding the skirt as the planner emits it (a
// table with a concurrent index): its UP drops the index, then the table; its
// inverse re-creates the table, then the index. Each inverse statement names
// the UP statement of the operation it undoes (`-- w17:skirt-op 1.k`).
func dropTableSkirt() *applyfetchpb.Migration {
	return &applyfetchpb.Migration{
		Id:    witnessMigrationID,
		UpSql: pendingInsert,
		UpPostTx: skirtWitnessStmt + `-- w17:skirt-op 1
DROP INDEX IF EXISTS "o_old_v_idx";
DROP TABLE IF EXISTS "o_old";
` + completeUpdate,
		DownPreTx: `-- w17:skirt-op 1
-- w17:skirt-op 1.1
CREATE TABLE "o_old" ("id" BIGINT PRIMARY KEY, "v" BIGINT);
-- w17:skirt-op 1.0
CREATE INDEX CONCURRENTLY "o_old_v_idx" ON "o_old" ("v");
` + ledgerErase,
	}
}

// A half-run operation of more than one statement: the DROP TABLE fails on a
// dependent view after its DROP INDEX ran. The rollback ran the operation's
// WHOLE inverse — CREATE TABLE of a table never dropped, 42P07 — and every
// retry stopped at the same statement.
func TestApplier_RollbackOfAHalfRunSkirtOperation_Live(t *testing.T) {
	ctx, _, a, conn := openSkirtTest(t)
	seedSQL(t, ctx, conn,
		`CREATE TABLE o_old (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO o_old VALUES (1, 10), (2, 20)`,
		`CREATE INDEX o_old_v_idx ON o_old (v)`, `CREATE VIEW o_old_view AS SELECT * FROM o_old`)
	m := dropTableSkirt()
	if err := a.Apply(ctx, m); err == nil || !strings.Contains(err.Error(), "statement 3 of 4") {
		t.Fatalf("want the skirt to stop at the DROP TABLE (statement 3 of 4): %v", err)
	}
	if got := renameSkirtCursor(t, ctx, conn); got != "2" {
		t.Fatalf("post_tx_done = %s, want 2", got)
	}
	if err := a.Rollback(ctx, m); err != nil {
		t.Fatalf("rollback of the half-run operation: %v", err)
	}
	if got := skirtRows(t, ctx, conn, `SELECT id, v FROM o_old ORDER BY id`); got != "1|10;2|20" {
		t.Fatalf("after the rollback o_old = %q", got)
	}
	if got := skirtRows(t, ctx, conn, `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('o_old_v_idx')`); got != "true" {
		t.Fatalf("after the rollback o_old_v_idx valid = %q, want true", got)
	}
	if got := skirtRows(t, ctx, conn, `SELECT count(*) FROM w17_migrations WHERE timestamp = '`+witnessMigrationTS+`'`); got != "0" {
		t.Fatalf("the ledger still holds the rolled-back migration: %s", got)
	}
	if head, err := a.AppliedHead(ctx); err != nil || head != prevMigrationID {
		t.Fatalf("AppliedHead = %q, %v; want %s", head, err, prevMigrationID)
	}
}

// The same operation run to its end is undone whole: both inverses run.
func TestApplier_RollbackOfAFullyRunMarkedOperation_Live(t *testing.T) {
	ctx, _, a, conn := openSkirtTest(t)
	seedSQL(t, ctx, conn,
		`CREATE TABLE o_old (id BIGINT PRIMARY KEY, v BIGINT)`, `CREATE INDEX o_old_v_idx ON o_old (v)`,
		`CREATE TABLE blocker (x BIGINT)`, `INSERT INTO blocker VALUES (NULL)`)
	m := dropTableSkirt()
	// A second operation after the drop fails, so the migration stays
	// half-applied with the whole of operation 1 recorded.
	m.UpPostTx = strings.Replace(m.UpPostTx, completeUpdate, "-- w17:skirt-op 2\nALTER TABLE blocker ALTER COLUMN x SET NOT NULL;\n"+completeUpdate, 1)
	m.DownPreTx = "-- w17:skirt-op 2\nALTER TABLE blocker ALTER COLUMN x DROP NOT NULL;\n" + m.DownPreTx
	if err := a.Apply(ctx, m); err == nil || !strings.Contains(err.Error(), "statement 4 of 5") {
		t.Fatalf("want the skirt to stop at the SET NOT NULL: %v", err)
	}
	if err := a.Rollback(ctx, m); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := skirtRows(t, ctx, conn, `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('o_old_v_idx')`); got != "true" {
		t.Fatalf("after the rollback o_old_v_idx valid = %q, want true", got)
	}
}

// concurrentDropIndexSkirt drops a UNIQUE concurrent index in the skirt; its
// inverse is emitAddIndex's pair (DROP … IF EXISTS, then the build).
func concurrentDropIndexSkirt() *applyfetchpb.Migration {
	return &applyfetchpb.Migration{
		Id:    witnessMigrationID,
		UpSql: pendingInsert,
		UpPostTx: skirtWitnessStmt + `-- w17:skirt-op 1
DROP INDEX CONCURRENTLY IF EXISTS "o_u_v_uidx";
` + completeUpdate,
		DownPreTx: `-- w17:skirt-op 1
DROP INDEX CONCURRENTLY IF EXISTS "o_u_v_uidx";
CREATE UNIQUE INDEX CONCURRENTLY "o_u_v_uidx" ON "o_u" ("v");
` + ledgerErase,
	}
}

// The rollback's concurrent build fails on the data (duplicates) and leaves an
// INVALID index; the rows are fixed and the rollback resumed at the build:
// `relation "o_u_v_uidx" already exists` on every retry. And while it stood
// stopped, the ledger's readers called the migration applied: the head, no
// pending row, phase complete — a deploy applied the next migration forward
// over the half-undone schema.
func TestApplier_StoppedRollbackOfACompleteMigration_Live(t *testing.T) {
	ctx, _, a, conn := openSkirtTest(t)
	seedSQL(t, ctx, conn,
		`CREATE TABLE o_u (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO o_u VALUES (1, 1), (2, 2)`,
		`CREATE UNIQUE INDEX o_u_v_uidx ON o_u (v)`)
	m := concurrentDropIndexSkirt()
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO o_u VALUES (3, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := a.Rollback(ctx, m); err == nil {
		t.Fatal("a unique build over duplicates rolled back")
	}

	t.Run("NotApplied", func(t *testing.T) {
		if head, err := a.AppliedHead(ctx); err != nil || head != prevMigrationID {
			t.Errorf("AppliedHead = %q, %v; want %s (the migration is being undone)", head, err, prevMigrationID)
		}
		if ids, err := a.PendingMigrations(ctx); err != nil || len(ids) != 1 || ids[0] != witnessMigrationID {
			t.Errorf("PendingMigrations = %v, %v; want [%s]", ids, err, witnessMigrationID)
		}
		if phase, err := a.MigrationPhase(ctx, witnessMigrationID); err != nil || phase != migrate.PhasePending {
			t.Errorf("MigrationPhase = %v, %v; want PhasePending", phase, err)
		}
		if err := a.ApplyPostTx(ctx, m); err == nil || !strings.Contains(err.Error(), "finish the rollback") {
			t.Errorf("a half-undone migration was resumed forward: %v", err)
		}
	})

	if _, err := conn.Exec(ctx, `DELETE FROM o_u WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	if err := a.Rollback(ctx, m); err != nil {
		t.Fatalf("the resumed rollback: %v", err)
	}
	if got := skirtRows(t, ctx, conn, `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('o_u_v_uidx')`); got != "true" {
		t.Fatalf("after the rollback o_u_v_uidx valid = %q, want true", got)
	}
	if got := skirtRows(t, ctx, conn, `SELECT count(*) FROM w17_migrations WHERE timestamp = '`+witnessMigrationTS+`'`); got != "0" {
		t.Fatalf("the ledger still holds the rolled-back migration: %s", got)
	}
	if head, err := a.AppliedHead(ctx); err != nil || head != prevMigrationID {
		t.Fatalf("AppliedHead = %q, %v; want %s", head, err, prevMigrationID)
	}
}

// signatureFooter is the line decorate appends to a served skirt.
const signatureFooter = "-- w17:content_signature: 00ff\n"

// The skirt as the console serves it ends with the signature footer, which
// split into a statement of its own: the pre-commit gate read the footer as
// the skirt's last statement, never saw the completing UPDATE, and let the
// in-tx half commit before the cursor column was found missing.
func TestCompletesAPendingRow_ServedSkirt(t *testing.T) {
	body := renameSkirtMigration(true).GetUpPostTx() + "\n" + signatureFooter
	if !postgres.CompletesAPendingRowForTest(postgres.SplitStatementsForTest(body)) {
		t.Fatal("a served skirt (signature footer last) is not recognised as completing a pending row")
	}
}

func TestApplier_RefusesAnUntrackableServedSkirtBeforeCommit_Live(t *testing.T) {
	ctx, dsn, admin, conn := openSkirtTest(t)
	seedRenameSkirt(t, ctx, conn)
	roleDSN := cursorlessDeployRole(t, ctx, dsn, conn)
	a, err := postgres.New(ctx, roleDSN)
	if err != nil {
		t.Fatalf("connect as the role: %v", err)
	}
	defer func() { _ = a.Close() }()
	m := renameSkirtMigration(false)
	m.UpSql = "-- w17:expected_pre_fingerprint: 00\n" + m.UpSql
	m.UpPostTx += "\n" + signatureFooter
	err = a.Apply(ctx, m)
	if err == nil || !strings.Contains(err.Error(), "nothing of this migration ran") {
		t.Fatalf("want the refusal before the in-tx half: %v", err)
	}
	if phase, err := admin.MigrationPhase(ctx, m.Id); err != nil || phase != migrate.PhaseFresh {
		t.Fatalf("phase = %v, %v; want PhaseFresh (nothing committed)", phase, err)
	}
	if got := skirtRows(t, ctx, conn, `SELECT count(*) FROM w17_migrations`); got != "1" {
		t.Fatalf("ledger rows = %s, want 1", got)
	}
}

// A rollback resumed at a CONCURRENTLY build that no DROP … IF EXISTS
// precedes (a DropTable's inverse re-creates the table, then its indexes)
// settles what a stopped rollback left of it: a build that finished before
// its record (the process died between the two) is skipped and recorded, an
// INVALID leftover of a failed one is dropped and built again. Both used to
// stop on `already exists` on every retry.
func TestApplier_RollbackResumeSettlesItsConcurrentBuild_Live(t *testing.T) {
	for _, leg := range []string{"Finished", "Interrupted"} {
		t.Run(leg, func(t *testing.T) {
			ctx, _, a, conn := openSkirtTest(t)
			seedSQL(t, ctx, conn,
				`CREATE TABLE o_old (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO o_old VALUES (1, 10), (2, 20)`,
				`CREATE UNIQUE INDEX o_old_v_idx ON o_old (v)`)
			m := dropTableSkirt()
			m.DownPreTx = strings.Replace(m.DownPreTx, `CREATE INDEX CONCURRENTLY`, `CREATE UNIQUE INDEX CONCURRENTLY`, 1)
			if err := a.Apply(ctx, m); err != nil {
				t.Fatalf("apply: %v", err)
			}
			// The rollback re-created the table (recorded), then stopped in
			// or right after the index build.
			stopped := []string{`CREATE TABLE "o_old" ("id" BIGINT PRIMARY KEY, "v" BIGINT)`, `INSERT INTO o_old VALUES (1, 10), (2, 20)`,
				`ALTER TABLE w17_migrations ADD COLUMN IF NOT EXISTS post_tx_undone INTEGER`,
				`UPDATE w17_migrations SET post_tx_undone = 1 WHERE timestamp = '` + witnessMigrationTS + `'`}
			if leg == "Finished" {
				stopped = append(stopped, `CREATE UNIQUE INDEX CONCURRENTLY o_old_v_idx ON o_old (v)`)
			} else {
				stopped = append(stopped, `INSERT INTO o_old VALUES (3, 10)`)
			}
			for _, q := range stopped {
				if _, err := conn.Exec(ctx, q, pgx.QueryExecModeSimpleProtocol); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if leg == "Interrupted" {
				if _, err := conn.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY o_old_v_idx ON o_old (v)`, pgx.QueryExecModeSimpleProtocol); err == nil {
					t.Fatal("the unique build over duplicates succeeded")
				}
				if got := skirtRows(t, ctx, conn, `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('o_old_v_idx')`); got != "false" {
					t.Fatalf("no INVALID leftover: %q", got)
				}
				if _, err := conn.Exec(ctx, `DELETE FROM o_old WHERE id = 3`); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Rollback(ctx, m); err != nil {
				t.Fatalf("the resumed rollback: %v", err)
			}
			if got := skirtRows(t, ctx, conn, `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('o_old_v_idx')`); got != "true" {
				t.Fatalf("after the rollback o_old_v_idx valid = %q, want true", got)
			}
			if got := skirtRows(t, ctx, conn, `SELECT count(*) FROM w17_migrations WHERE timestamp = '`+witnessMigrationTS+`'`); got != "0" {
				t.Fatalf("the ledger still holds the rolled-back migration: %s", got)
			}
		})
	}
}

// The same half-run DropTable minted before the inverses named their UP
// statement: the rollback runs the operation's whole inverse, and its failure
// says so and names the hand step that skips the inverse of the statement
// that never ran — which then finishes the rollback.
func TestApplier_RollbackOfAHalfRunUnnamedOperationNamesTheWayOut_Live(t *testing.T) {
	ctx, _, a, conn := openSkirtTest(t)
	seedSQL(t, ctx, conn,
		`CREATE TABLE o_old (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO o_old VALUES (1, 10), (2, 20)`,
		`CREATE INDEX o_old_v_idx ON o_old (v)`, `CREATE VIEW o_old_view AS SELECT * FROM o_old`)
	m := dropTableSkirt()
	m.DownPreTx = strings.NewReplacer("-- w17:skirt-op 1.1\n", "", "-- w17:skirt-op 1.0\n", "").Replace(m.DownPreTx)
	if err := a.Apply(ctx, m); err == nil {
		t.Fatal("the DROP TABLE under a dependent view ran")
	}
	err := a.Rollback(ctx, m)
	hint := `UPDATE w17_migrations SET post_tx_undone = 1 WHERE timestamp = '` + witnessMigrationTS + `';`
	for _, want := range []string{"stopped after 1 of its 2 statement(s)", "whole inverse", hint} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the failed rollback does not say %q:\n%v", want, err)
		}
	}
	// T1-3 pass #6 review 1, R1-1: an unnamed inverse is not proof of an
	// older console — an emitter that names nothing lands here too — so the
	// message must not say when the migration was minted.
	if strings.Contains(err.Error(), "minted") {
		t.Fatalf("the failed rollback asserts when the migration was minted:\n%v", err)
	}
	if _, err := conn.Exec(ctx, hint); err != nil {
		t.Fatal(err)
	}
	if err := a.Rollback(ctx, m); err != nil {
		t.Fatalf("the rollback after the hand step: %v", err)
	}
	if got := skirtRows(t, ctx, conn, `SELECT id, v FROM o_old ORDER BY id`); got != "1|10;2|20" {
		t.Fatalf("after the rollback o_old = %q", got)
	}
	if !indexExists(t, ctx, conn, "o_old_v_idx") {
		t.Fatal("the rollback did not re-create the dropped index")
	}
}
