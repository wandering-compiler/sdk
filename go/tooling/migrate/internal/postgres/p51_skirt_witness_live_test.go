package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// T1-1 pass #51, Q51-1 (C51-1 + C51-2 + C51-9) and owner ruling O51-2.
//
// The released pair — console d133b6a04 with sdk af86452 — mints the skirt
// flood (`DROP TABLE IF EXISTS "old_t"` ahead of `ALTER TABLE "t2" RENAME TO
// "old_t"`) and runs it with an applier that records no resume cursor. A
// deploy that stops on the data (a SET NOT NULL over a NULL) leaves a pending
// row whose cursor is NULL with statements 1..k-1 run. The cursor-aware
// applier took that NULL as 0 on the next deploy: it ran the skirt again from
// the top and the DROP TABLE dropped the renamed table with its rows; the
// rollback then trusted the cursor the resume had written and returned nil
// with the rows gone.

const p51TS = "2026-02-01T00:00:00Z"
const p51ID = "20260201T000000Z"

// p51Witness is the statement the console mints at the head of every skirt
// (emit/postgres RenderW17MigrationsSkirtWitness): an applier that does not
// record the cursor runs it and stops there, before any DDL of the skirt; an
// applier that does recognises it and records it done without running it.
const p51Witness = `-- w17:skirt-witness
DO $w17_skirt$ BEGIN RAISE EXCEPTION 'w17: migration ` + p51TS + ` must be run by an applier that records how far its non-transactional part got; this one does not, and a retry from its first statement can drop a table a later statement renamed into place. Upgrade w17ctl or the service binary (sdk/go with the skirt resume cursor) and deploy again: nothing of this part has run.'; END $w17_skirt$;
`

// p51RenameSkirt is the all-post-tx migration the console mints when t2 takes
// the name of old_t, which it drops, next to a concurrent index — operation
// markers in both directions, as applied.Wrap lays it out. witness: minted by
// a console that heads the skirt with the witness statement; without it, one
// minted before (today's production).
func p51RenameSkirt(witness bool) *applyfetchpb.Migration {
	w := ""
	if witness {
		w = p51Witness
	}
	return &applyfetchpb.Migration{
		Id: p51ID,
		UpSql: "BEGIN;\n\nINSERT INTO \"w17_migrations\" (\"timestamp\", \"content_sha256\", \"post_tx_complete\") VALUES ('" + p51TS + "', 'cc', false);\n\nCOMMIT;\n",
		UpPostTx: w + `-- w17:skirt-op 1
DROP INDEX CONCURRENTLY IF EXISTS "old_t_x_idx";
DROP TABLE IF EXISTS "old_t";
-- w17:skirt-op 2
ALTER TABLE "t2" RENAME TO "old_t";
-- w17:skirt-op 3
ALTER TABLE "old_t" ALTER COLUMN "v" SET NOT NULL;
-- w17:skirt-op 4
DROP INDEX CONCURRENTLY IF EXISTS "old_t_v_idx";
CREATE INDEX CONCURRENTLY "old_t_v_idx" ON "old_t" ("v");
UPDATE "w17_migrations" SET "post_tx_complete" = true WHERE "timestamp" = '` + p51TS + `';`,
		DownPreTx: `-- w17:skirt-op 4
DROP INDEX CONCURRENTLY IF EXISTS "old_t_v_idx";
-- w17:skirt-op 3
ALTER TABLE "old_t" ALTER COLUMN "v" DROP NOT NULL;
-- w17:skirt-op 2
ALTER TABLE "old_t" RENAME TO "t2";
-- w17:skirt-op 1
CREATE TABLE "old_t" ("id" BIGINT PRIMARY KEY, "x" BIGINT);
CREATE INDEX CONCURRENTLY "old_t_x_idx" ON "old_t" ("x");
-- w17:skirt-op always
DELETE FROM "w17_migrations" WHERE "timestamp" = '` + p51TS + `';`,
	}
}

// p51Seed is the database before the migration: old_t (one row, a concurrent
// index) and t2 with three rows, one NULL.
func p51Seed(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	for _, q := range []string{
		ledgerCreate,
		`INSERT INTO w17_migrations (timestamp, content_sha256) VALUES ('2026-01-01T00:00:00Z', 'aa')`,
		`CREATE TABLE old_t (id BIGINT PRIMARY KEY, x BIGINT)`, `CREATE INDEX old_t_x_idx ON old_t (x)`, `INSERT INTO old_t VALUES (1, 7)`,
		`CREATE TABLE t2 (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO t2 VALUES (1, 10), (2, 20), (3, NULL)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// runLikeTheReleasedApplier runs m the way sdk af86452 does: the in-tx body,
// then every skirt statement in order in autocommit, recording nothing, and
// stops at the first that fails. It never adds the cursor column.
func runLikeTheReleasedApplier(ctx context.Context, conn *pgx.Conn, m *applyfetchpb.Migration, skirtOnly bool) error {
	if !skirtOnly {
		if _, err := conn.Exec(ctx, m.GetUpSql(), pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("up_sql: %w", err)
		}
	}
	for _, stmt := range postgres.SplitStatementsForTest(m.GetUpPostTx()) {
		if _, err := conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("postgres apply up_post_tx: %w", err)
		}
	}
	return nil
}

func p51Open(t *testing.T) (context.Context, string, *postgres.Applier, *pgx.Conn) {
	t.Helper()
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return ctx, dsn, a, conn
}

func p51Cursor(t *testing.T, ctx context.Context, conn *pgx.Conn) string {
	t.Helper()
	return skirtRows(t, ctx, conn, `SELECT post_tx_done FROM w17_migrations WHERE timestamp = '`+p51TS+`'`)
}

// Q51-1 — a resume over the released applier's half-run (pending row, NULL
// cursor, a migration minted without the witness) refuses and names the way
// out; it neither re-runs the drop nor writes a cursor. Then both ways out
// are taken: record the cursor as the text says and deploy again, or roll
// back — which, with nothing recorded, runs every inverse and gives t2 back
// with its rows (C51-2).
func TestQ51_1_ResumeOverAnUnrecordedHalfRunRefuses_Live(t *testing.T) {
	for _, way := range []string{"RecordAndResume", "Rollback"} {
		t.Run(way, func(t *testing.T) {
			ctx, _, a, conn := p51Open(t)
			p51Seed(t, ctx, conn)
			m := p51RenameSkirt(false)
			if err := runLikeTheReleasedApplier(ctx, conn, m, false); err == nil || !strings.Contains(err.Error(), "23502") {
				t.Fatalf("the released applier's run should stop at the SET NOT NULL: %v", err)
			}
			if got := skirtRows(t, ctx, conn, `SELECT id, v FROM old_t ORDER BY id`); got != "1|10;2|20;3|null" {
				t.Fatalf("after the released applier old_t = %q", got)
			}

			if phase, err := a.MigrationPhase(ctx, m.Id); err != nil || phase != migrate.PhasePending {
				t.Fatalf("phase = %v, %v; want PhasePending", phase, err)
			}
			err := a.ApplyPostTx(ctx, m)
			if err == nil {
				t.Fatal("the resume over an unrecorded half-run went ahead")
			}
			for _, want := range []string{"records no resume point", "post_tx_done", "deploy again", `migrate rollback --to "20260101T000000Z"`, "statement 3:"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if got := skirtRows(t, ctx, conn, `SELECT id, v FROM old_t ORDER BY id`); got != "1|10;2|20;3|null" {
				t.Fatalf("after the refused resume old_t = %q, want the renamed rows — the resume re-ran the drop of the freed name", got)
			}
			if got := p51Cursor(t, ctx, conn); got != "null" {
				t.Fatalf("the refused resume recorded a cursor %q; a rollback would trust it", got)
			}

			switch way {
			case "RecordAndResume":
				// The text's way out: fix the row, record how many ran (3),
				// deploy again.
				for _, q := range []string{`UPDATE old_t SET v = 30 WHERE v IS NULL`,
					`UPDATE w17_migrations SET post_tx_done = 3 WHERE timestamp = '` + p51TS + `'`} {
					if _, err := conn.Exec(ctx, q); err != nil {
						t.Fatalf("%s: %v", q, err)
					}
				}
				if err := a.ApplyPostTx(ctx, m); err != nil {
					t.Fatalf("resume after recording the cursor: %v", err)
				}
				if got := skirtRows(t, ctx, conn, `SELECT id, v FROM old_t ORDER BY id`); got != "1|10;2|20;3|30" {
					t.Fatalf("after the resume old_t = %q", got)
				}
				if phase, err := a.MigrationPhase(ctx, m.Id); err != nil || phase != migrate.PhaseComplete {
					t.Fatalf("phase = %v, %v; want PhaseComplete", phase, err)
				}
			case "Rollback":
				if err := a.Rollback(ctx, m); err != nil {
					t.Fatalf("rollback: %v", err)
				}
				if got := skirtRows(t, ctx, conn, `SELECT id, v FROM t2 ORDER BY id`); got != "1|10;2|20;3|null" {
					t.Fatalf("after the rollback t2 = %q, want its three rows back", got)
				}
				if got := skirtRows(t, ctx, conn, `SELECT count(*) FROM w17_migrations WHERE timestamp = '`+p51TS+`'`); got != "0" {
					t.Fatalf("the ledger still holds the rolled-back migration: %s", got)
				}
			}
		})
	}
}

// Q51-1 / C51-9 — the witness. The released applier runs a skirt minted with
// it and stops at the witness, before any DDL: losslessly, saying to
// upgrade. The cursor-aware applier then runs it from the top (a NULL under a
// witness means nothing ran), records the witness without running it, and
// the documented "fix the row and deploy again" completes it.
func TestQ51_1_WitnessStopsAnOlderApplierBeforeAnyDDL_Live(t *testing.T) {
	ctx, _, a, conn := p51Open(t)
	p51Seed(t, ctx, conn)
	m := p51RenameSkirt(true)
	err := runLikeTheReleasedApplier(ctx, conn, m, false)
	if err == nil || !strings.Contains(err.Error(), "Upgrade w17ctl or the service binary") {
		t.Fatalf("the released applier should stop at the witness: %v", err)
	}
	if got := skirtRows(t, ctx, conn, `SELECT id, x FROM old_t ORDER BY id`); got != "1|7" {
		t.Fatalf("the released applier ran past the witness: old_t = %q", got)
	}
	if got := skirtRows(t, ctx, conn, `SELECT id, v FROM t2 ORDER BY id`); got != "1|10;2|20;3|null" {
		t.Fatalf("the released applier ran past the witness: t2 = %q", got)
	}
	// Its own retry stops at the same place.
	if err := runLikeTheReleasedApplier(ctx, conn, m, true); err == nil || !strings.Contains(err.Error(), "nothing of this part has run") {
		t.Fatalf("the released applier's retry should stop at the witness again: %v", err)
	}

	err = a.ApplyPostTx(ctx, m)
	if err == nil || !strings.Contains(err.Error(), "statement 5 of 8") {
		t.Fatalf("the cursor-aware resume should run from the top and stop at the SET NOT NULL (statement 5 of 8): %v", err)
	}
	if got := p51Cursor(t, ctx, conn); got != "4" {
		t.Fatalf("cursor = %s, want 4 (the witness and three statements)", got)
	}
	if _, err := conn.Exec(ctx, `UPDATE old_t SET v = 30 WHERE v IS NULL`); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyPostTx(ctx, m); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := skirtRows(t, ctx, conn, `SELECT id, v FROM old_t ORDER BY id`); got != "1|10;2|20;3|30" {
		t.Fatalf("after the resume old_t = %q", got)
	}
	if !indexExists(t, ctx, conn, "old_t_v_idx") {
		t.Fatal("the concurrent index was not built")
	}
}

// The control V3 measured: the cursor-aware applier's own crash right after
// the in-tx COMMIT leaves the same ledger state (pending, NULL). Under the
// witness that state resumes from the top, as it must; and a rollback from it
// runs only the inverses that always run.
func TestQ51_1_WitnessFreshCrashResumesAndRollsBack_Live(t *testing.T) {
	for _, way := range []string{"Resume", "Rollback"} {
		t.Run(way, func(t *testing.T) {
			ctx, _, a, conn := p51Open(t)
			p51Seed(t, ctx, conn)
			if _, err := conn.Exec(ctx, `UPDATE t2 SET v = 30 WHERE v IS NULL`); err != nil {
				t.Fatal(err)
			}
			m := p51RenameSkirt(true)
			if err := a.Apply(ctx, &applyfetchpb.Migration{Id: m.Id, UpSql: m.UpSql}); err != nil {
				t.Fatalf("in-tx half: %v", err)
			}
			if way == "Resume" {
				if err := a.ApplyPostTx(ctx, m); err != nil {
					t.Fatalf("resume after a fresh crash: %v", err)
				}
				if got := skirtRows(t, ctx, conn, `SELECT id, v FROM old_t ORDER BY id`); got != "1|10;2|20;3|30" {
					t.Fatalf("after the resume old_t = %q", got)
				}
				return
			}
			if err := a.Rollback(ctx, m); err != nil {
				t.Fatalf("rollback of a skirt nothing of which ran: %v", err)
			}
			if got := skirtRows(t, ctx, conn, `SELECT id, x FROM old_t ORDER BY id`); got != "1|7" {
				t.Fatalf("after the rollback old_t = %q, want the untouched original", got)
			}
			if got := skirtRows(t, ctx, conn, `SELECT id, v FROM t2 ORDER BY id`); got != "1|10;2|20;3|30" {
				t.Fatalf("after the rollback t2 = %q", got)
			}
			if got := skirtRows(t, ctx, conn, `SELECT count(*) FROM w17_migrations WHERE timestamp = '`+p51TS+`'`); got != "0" {
				t.Fatalf("the ledger still holds the rolled-back migration: %s", got)
			}
		})
	}
}

// p51Role creates a deploy role that may write the ledger but not ALTER it
// (the ledger belongs to the superuser), and owns the application tables —
// the split C51-3 measured (a DBA-created ledger, a rotated role).
func p51Role(t *testing.T, ctx context.Context, dsn string, conn *pgx.Conn) string {
	t.Helper()
	role := fmt.Sprintf("w17_dep_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		admin, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE IF EXISTS `+role, pgx.QueryExecModeSimpleProtocol)
			_ = admin.Close(context.Background())
		}
	})
	if _, err := conn.Exec(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'p';
		GRANT SELECT, INSERT, UPDATE, DELETE ON w17_migrations TO `+role+`;
		GRANT CREATE ON SCHEMA public TO `+role+`;
		ALTER TABLE old_t OWNER TO `+role+`; ALTER TABLE t2 OWNER TO `+role+`;`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "p")
	return u.String()
}

// O51-2 — an applier that cannot track the cursor (no ALTER on the ledger)
// refuses and names the missing privilege. On a fresh apply it refuses before
// the in-tx half, so nothing is left pending; on a resume it refuses before
// any skirt statement. It used to warn and run the skirt untracked.
func TestO51_2_ALedgerTheRoleCannotAlterRefuses_Live(t *testing.T) {
	for _, leg := range []string{"Apply", "Resume"} {
		t.Run(leg, func(t *testing.T) {
			ctx, dsn, admin, conn := p51Open(t)
			p51Seed(t, ctx, conn)
			if _, err := conn.Exec(ctx, `UPDATE t2 SET v = 30 WHERE v IS NULL`); err != nil {
				t.Fatal(err)
			}
			roleDSN := p51Role(t, ctx, dsn, conn)
			a, err := postgres.New(ctx, roleDSN)
			if err != nil {
				t.Fatalf("connect as the role: %v", err)
			}
			defer func() { _ = a.Close() }()
			// Minted before the witness, so nothing in the skirt itself
			// would stop an untracked run.
			m := p51RenameSkirt(false)
			var applyErr error
			if leg == "Apply" {
				applyErr = a.Apply(ctx, m)
			} else {
				if err := admin.Apply(ctx, &applyfetchpb.Migration{Id: m.Id, UpSql: m.UpSql}); err != nil {
					t.Fatalf("in-tx half: %v", err)
				}
				if _, err := conn.Exec(ctx, `ALTER TABLE w17_migrations DROP COLUMN IF EXISTS post_tx_done`); err != nil {
					t.Fatal(err)
				}
				applyErr = a.ApplyPostTx(ctx, m)
			}
			if applyErr == nil {
				t.Fatal("the skirt ran without a resume point")
			}
			for _, want := range []string{"post_tx_done", "owner", "ALTER TABLE w17_migrations OWNER TO"} {
				if !strings.Contains(applyErr.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, applyErr)
				}
			}
			if got := skirtRows(t, ctx, conn, `SELECT id, x FROM old_t ORDER BY id`); got != "1|7" {
				t.Fatalf("the refused skirt ran: old_t = %q", got)
			}
			wantPhase := migrate.PhasePending
			if leg == "Apply" {
				wantPhase = migrate.PhaseFresh
			}
			if phase, err := admin.MigrationPhase(ctx, m.Id); err != nil || phase != wantPhase {
				t.Fatalf("phase = %v, %v; want %v", phase, err, wantPhase)
			}
		})
	}
}

// Q51-4 (C51-10) — the failure text counts the cursor the ledger holds, not
// the index of the failing statement. A `DROP INDEX CONCURRENTLY IF EXISTS`
// ahead of a concurrent build is not recorded on its own, so when the build
// fails the ledger still says 0 and the resume starts at the drop; the text
// said "1 statement(s) … recorded as done" and "resumes at this statement".
func TestQ51_4_SkirtFailureCountsTheRecordedCursor_Live(t *testing.T) {
	ctx, _, a, conn := p51Open(t)
	for _, q := range []string{ledgerCreate,
		`INSERT INTO w17_migrations (timestamp, content_sha256) VALUES ('2026-01-01T00:00:00Z', 'aa')`,
		`CREATE TABLE d (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO d VALUES (1, 5), (2, 5)`} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	m := &applyfetchpb.Migration{
		Id:    p51ID,
		UpSql: "BEGIN;\n\nINSERT INTO \"w17_migrations\" (\"timestamp\", \"content_sha256\", \"post_tx_complete\") VALUES ('" + p51TS + "', 'cc', false);\n\nCOMMIT;\n",
		UpPostTx: `DROP INDEX CONCURRENTLY IF EXISTS "d_v_key";
CREATE UNIQUE INDEX CONCURRENTLY "d_v_key" ON "d" ("v");
UPDATE "w17_migrations" SET "post_tx_complete" = true WHERE "timestamp" = '` + p51TS + `';`,
	}
	err := a.Apply(ctx, m)
	if err == nil {
		t.Fatal("a unique index over duplicates was built")
	}
	if got := p51Cursor(t, ctx, conn); got != "0" {
		t.Fatalf("cursor = %s, want 0", got)
	}
	for _, want := range []string{"statement 2 of 3", "first 0 statement(s)", "resumes at statement 1", "statements 1–1 ran too and are not recorded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not say %q:\n%v", want, err)
		}
	}
}

// Q51-2 (C51-3, text leg) — the legacy layout (an all-post-tx migration minted
// before it got a pending row; its LAST statement is the ledger INSERT) has
// no resume point, and its failure text told the operator to run statements
// k..n by hand — the ledger INSERT among them — and then `migrate apply
// --fake`, whose INSERT then meets the same primary key (23505).
func TestQ51_2_LegacySkirtFailureNamesTheLedgerStatement_Live(t *testing.T) {
	ctx, _, a, conn := p51Open(t)
	for _, q := range []string{ledgerCreate,
		`INSERT INTO w17_migrations (timestamp, content_sha256) VALUES ('2026-01-01T00:00:00Z', 'aa')`,
		`CREATE TABLE d (id BIGINT PRIMARY KEY, v BIGINT, w BIGINT)`, `INSERT INTO d VALUES (1, NULL, 1)`} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	m := &applyfetchpb.Migration{
		Id: p51ID,
		UpPostTx: `CREATE INDEX CONCURRENTLY "d_w_idx" ON "d" ("w");
ALTER TABLE "d" ALTER COLUMN "v" SET NOT NULL;
CREATE INDEX CONCURRENTLY "d_v_idx" ON "d" ("v");
INSERT INTO "w17_migrations" ("timestamp", "content_sha256") VALUES ('` + p51TS + `', 'cc');`,
	}
	err := a.Apply(ctx, m)
	if err == nil {
		t.Fatal("SET NOT NULL over a NULL applied")
	}
	for _, want := range []string{"statement 2 of 4", "written by its last statement (4)", "run statements 2–3 of its up_post_tx by hand, then statement 4, the ledger INSERT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not say %q:\n%v", want, err)
		}
	}
	for _, bad := range []string{"--fake", "statements 2–4"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("the failure still says %q:\n%v", bad, err)
		}
	}
}
