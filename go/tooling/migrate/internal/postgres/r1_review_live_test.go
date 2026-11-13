package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// T1-1 pass #50, review R1-6 — Wipe forgot that the ledger had the phase
// column but not that it had the cursor column. One Applier: a two-phase
// apply (the cursor column is added, and cached), a Wipe, a ledger created
// again WITHOUT post_tx_done (its CREATE TABLE is the migration's own DDL),
// and the next two-phase apply skipped the ALTER and refused the deploy:
// `postgres post-tx resume point: … column "post_tx_done" does not exist`.
func TestApplier_WipeForgetsTheCursorColumn_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	build := func() {
		t.Helper()
		if err := a.Apply(ctx, plainMigration("20260101T000000Z", "2026-01-01T00:00:00Z")); err != nil {
			t.Fatalf("Apply plain: %v", err)
		}
		if err := a.Apply(ctx, skirtMigration("20260201T000000Z", "2026-02-01T00:00:00Z")); err != nil {
			t.Fatalf("Apply two-phase: %v", err)
		}
	}
	build()
	if err := a.Wipe(ctx, nil); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	build()
}

// T1-1 pass #50, review R1-3 — a skirt that stopped at statement k: the
// promised way out, `migrate rollback --to`, ran the WHOLE down_pre_tx, so the
// inverse of an operation that never ran failed (here the inverse of the
// unrun `ALTER TABLE t2 RENAME TO t` renames t — still the old table — onto
// t2, which still exists: 42P07) and the rollback stopped half-way too. With
// the migrator's operation markers, the rollback of a half-applied skirt runs
// only the inverses of the operations the cursor records as started; without
// them (a migration minted before) it runs every inverse, and a failing one
// says why and what to do.
func r1HalfAppliedSkirt(marked bool) *applyfetchpb.Migration {
	const ts = "2026-02-01T00:00:00Z"
	op := func(n string) string {
		if !marked {
			return ""
		}
		return "-- w17:skirt-op " + n + "\n"
	}
	return &applyfetchpb.Migration{
		Id: "20260201T000000Z",
		UpSql: `BEGIN;` + ledgerCreate + `
			INSERT INTO w17_migrations (timestamp, content_sha256, post_tx_complete) VALUES ('` + ts + `', 'cc', false);
			COMMIT;`,
		// Two groups of a skirt in differ order: a's index and NOT NULL (the
		// NOT NULL fails on the NULL), then t dropped and t2 renamed onto its
		// name.
		UpPostTx: op("1") + `DROP INDEX CONCURRENTLY IF EXISTS a_v_idx;
CREATE INDEX CONCURRENTLY a_v_idx ON a (v);
` + op("2") + `ALTER TABLE a ALTER COLUMN v SET NOT NULL;
` + op("3") + `DROP TABLE IF EXISTS t;
` + op("4") + `ALTER TABLE t2 RENAME TO t;
UPDATE w17_migrations SET post_tx_complete = true WHERE timestamp = '` + ts + `';`,
		// applied.Wrap's all-post-tx layout: the inverses in reverse order,
		// then the ledger erase.
		DownPreTx: op("4") + `ALTER TABLE t RENAME TO t2;
` + op("3") + `CREATE TABLE t (id BIGINT PRIMARY KEY);
` + op("2") + `ALTER TABLE a ALTER COLUMN v DROP NOT NULL;
` + op("1") + `DROP INDEX CONCURRENTLY IF EXISTS a_v_idx;
` + op("always") + `DELETE FROM w17_migrations WHERE timestamp = '` + ts + `';`,
	}
}

func TestApplier_RollbackOfAHalfAppliedSkirtSkipsTheUnrunInverses_Live(t *testing.T) {
	for _, marked := range []bool{true, false} {
		name := "Marked"
		if !marked {
			name = "Unmarked"
		}
		t.Run(name, func(t *testing.T) {
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
				`CREATE TABLE a (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO a VALUES (1, NULL)`,
				`CREATE TABLE t (id BIGINT PRIMARY KEY)`, `CREATE TABLE t2 (id BIGINT PRIMARY KEY)`,
			} {
				if _, err := conn.Exec(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			m := r1HalfAppliedSkirt(marked)
			err = a.Apply(ctx, m)
			if err == nil || !strings.Contains(err.Error(), "statement 3 of 6") {
				t.Fatalf("want the skirt to stop at statement 3 of 6: %v", err)
			}
			err = a.Rollback(ctx, m)
			if !marked {
				for _, want := range []string{"half-applied", "does not mark its operations", "EVERY inverse", "undo by hand"} {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("the failed rollback of an unmarked half-applied skirt does not say %q: %v", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("rollback of the half-applied migration: %v", err)
			}
			for _, q := range []string{`SELECT to_regclass('t') IS NOT NULL AND to_regclass('t2') IS NOT NULL`, `SELECT to_regclass('a_v_idx') IS NULL`} {
				var ok bool
				if err := conn.QueryRow(ctx, q).Scan(&ok); err != nil || !ok {
					t.Errorf("after the rollback %s = %v (%v), want true", q, ok, err)
				}
			}
			var n int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM w17_migrations WHERE timestamp = '2026-02-01T00:00:00Z'`).Scan(&n); err != nil || n != 0 {
				t.Errorf("the ledger still holds the rolled-back migration (%d, %v)", n, err)
			}
		})
	}
}

// R1-3 — a cursor no cursor-aware applier recorded (NULL: an older applier
// ran part of the skirt and recorded nothing) is not trusted: every inverse
// runs, as before, and the failure says why.
func TestApplier_RollbackTrustsOnlyARecordedCursor_Live(t *testing.T) {
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
		`CREATE TABLE a (id BIGINT PRIMARY KEY, v BIGINT)`, `INSERT INTO a VALUES (1, NULL)`,
		`CREATE TABLE t (id BIGINT PRIMARY KEY)`, `CREATE TABLE t2 (id BIGINT PRIMARY KEY)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	m := r1HalfAppliedSkirt(true)
	if err := a.Apply(ctx, m); err == nil {
		t.Fatal("the skirt's SET NOT NULL applied over a NULL")
	}
	var done *int
	if err := conn.QueryRow(ctx, `SELECT post_tx_done FROM w17_migrations WHERE timestamp = '2026-02-01T00:00:00Z'`).Scan(&done); err != nil || done == nil || *done != 2 {
		t.Fatalf("post_tx_done = %v (%v), want 2", done, err)
	}
	if _, err := conn.Exec(ctx, `UPDATE w17_migrations SET post_tx_done = NULL WHERE timestamp = '2026-02-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	err = a.Rollback(ctx, m)
	if err == nil || !strings.Contains(err.Error(), "EVERY inverse") {
		t.Fatalf("a rollback over an unrecorded cursor must run every inverse and say so: %v", err)
	}
}
