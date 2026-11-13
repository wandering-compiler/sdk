package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// The skirt's resume cursor (T1-1 pass #50, P50-18).
//
// A skirt runs one statement at a time after the transaction's COMMIT, and a
// deploy can stop between any two of them: a statement fails on the data (a
// SET NOT NULL over a NULL, a unique index over duplicates — the "fix the rows
// and deploy again" flow), or the process dies. The resume used to run the
// skirt again from its FIRST statement. That was safe while a skirt held only
// a concurrent index's `DROP INDEX CONCURRENTLY IF EXISTS` + `CREATE INDEX
// CONCURRENTLY`; it is not since a skirt carries the DDL coupled to such an
// index (a-skirt-carries-only-schema-ddl): a re-run DROP COLUMN fails 42703
// forever, and the `DROP TABLE IF EXISTS "t"` ahead of `ALTER TABLE "t2"
// RENAME TO "t"` drops the renamed table — rows and all — on the ordinary
// retry.
//
// So the ledger row records how many of the skirt's statements are done
// (`post_tx_done`), and the resume starts after them:
//
//   - a statement that can run in a transaction runs in one WITH the cursor
//     update, so the two commit together: no crash can leave it done and
//     unrecorded;
//   - a CONCURRENTLY statement cannot; it runs on its own and the cursor is
//     moved right after it. The window between the two is a re-run of that
//     one statement, which the emitter makes harmless: a concurrent build is
//     preceded by `DROP INDEX CONCURRENTLY IF EXISTS` of its name, and the
//     cursor is NOT moved past such a drop, so a resume re-runs the pair (an
//     INVALID leftover of a crashed build is dropped, never frozen).
//
// The cursor lives only where a pending row does (the two-phase layout). A
// skirt without one — the legacy layout, an all-post-tx migration minted
// before it got its own pending row — runs as it always did, and its failure
// says that a new deploy would start it from the first statement.

// concurrentStmtRe matches a statement PostgreSQL refuses inside a
// transaction block because it runs CONCURRENTLY. Leading comment lines and
// whitespace are skipped first (effectiveHead).
var concurrentStmtRe = regexp.MustCompile(`(?is)^(?:CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY|DROP\s+INDEX\s+CONCURRENTLY|REINDEX\s+.*\bCONCURRENTLY\b|ALTER\s+TABLE\s+.*\bDETACH\s+PARTITION\s+.*\bCONCURRENTLY\b)`)

// rerunSafeDropRe matches a drop that is safe to run again whatever ran after
// it: `DROP INDEX [CONCURRENTLY] IF EXISTS`. The cursor is not moved past one
// that runs on its own, so the concurrent build after it is re-run with it.
var rerunSafeDropRe = regexp.MustCompile(`(?is)^DROP\s+INDEX\s+(?:CONCURRENTLY\s+)?IF\s+EXISTS\b`)

// effectiveHead is a statement with its leading whitespace and comments
// removed — what PostgreSQL parses first.
func effectiveHead(stmt string) string {
	s := strings.TrimSpace(stmt)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = strings.TrimSpace(s[i+1:])
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return ""
			}
			s = strings.TrimSpace(s[i+2:])
		default:
			return s
		}
	}
}

// runSkirt runs the skirt's statements from the first one the ledger does not
// record as done. tracked: the migration has a pending ledger row to hold the
// cursor (the caller re-checked the phase under the skirt's lock).
func (a *Applier) runSkirt(ctx context.Context, m *applyfetchpb.Migration, stmts []string, tracked bool) error {
	done := 0
	if tracked {
		if err := a.ensureCursorColumn(ctx); err != nil {
			// A ledger the applier may write but not ALTER: run as before
			// (from the top) rather than refuse the deploy, and say so.
			slog.Warn("postgres: the ledger has no post_tx_done column and it could not be added; this migration's non-transactional part runs without a resume point",
				slog.String("migration_id", m.GetId()), slog.String("error", err.Error()))
			tracked = false
		}
	}
	if tracked {
		var err error
		if done, err = a.skirtDone(ctx, m.GetId()); err != nil {
			return fmt.Errorf("postgres post-tx resume point: %w", err)
		}
		if done > len(stmts) {
			return fmt.Errorf("postgres apply up_post_tx: the ledger records %d statements of migration %s's non-transactional part as done, and it has %d — this is not the migration that ran here", done, m.GetId(), len(stmts))
		}
	}
	for i := done; i < len(stmts); i++ {
		stmt := stmts[i]
		head := effectiveHead(stmt)
		var err error
		switch {
		case !tracked:
			_, err = a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol)
		case concurrentStmtRe.MatchString(head):
			err = a.runAlone(ctx, m.GetId(), stmt, i+1, !rerunSafeDropRe.MatchString(head))
		default:
			err = a.runWithCursor(ctx, m.GetId(), stmt, i+1)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "25001" {
				// active_sql_transaction: a statement PostgreSQL refuses in a
				// transaction block that the pattern above does not know.
				err = a.runAlone(ctx, m.GetId(), stmt, i+1, true)
			}
		}
		if err != nil {
			return a.skirtFailure(ctx, m, stmts, i, done, tracked, err)
		}
	}
	return nil
}

// runWithCursor runs one statement and records it done, in one transaction.
func (a *Applier) runWithCursor(ctx context.Context, id, stmt string, n int) error {
	tx, err := a.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if err := setSkirtDone(ctx, tx, id, n); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// runAlone runs one statement in autocommit, then (advance) records it done.
func (a *Applier) runAlone(ctx context.Context, id, stmt string, n int, advance bool) error {
	if _, err := a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if !advance {
		return nil
	}
	return setSkirtDone(ctx, a.conn, id, n)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// skirtRowWhere selects a migration's ledger row by its id, canonicalised
// exactly as MigrationPhase reads it.
const skirtRowWhere = `to_char(timestamp AT TIME ZONE 'UTC', 'YYYYMMDD"T"HH24MISS"Z"') = $2`

func setSkirtDone(ctx context.Context, e execer, id string, n int) error {
	_, err := e.Exec(ctx, `UPDATE w17_migrations SET post_tx_done = $1 WHERE `+skirtRowWhere, n, id)
	return err
}

func (a *Applier) skirtDone(ctx context.Context, id string) (int, error) {
	var n int
	err := a.conn.QueryRow(ctx, `SELECT post_tx_done FROM w17_migrations WHERE to_char(timestamp AT TIME ZONE 'UTC', 'YYYYMMDD"T"HH24MISS"Z"') = $1`, id).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// ensureCursorColumn adds the `post_tx_done` column to the ledger when it is
// missing — the ledger's CREATE TABLE (a migration's own DDL) predates it,
// and every row without it reads 0 (nothing of its skirt recorded, which is
// what an older applier left). Asks the catalogue first, like
// ensurePhaseColumn, so a ledger that has it is never ALTERed.
func (a *Applier) ensureCursorColumn(ctx context.Context) error {
	if a.cursorColEnsured {
		return nil
	}
	var has bool
	if err := a.conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_attribute
		   WHERE attrelid = to_regclass('w17_migrations')
		     AND attname = 'post_tx_done' AND NOT attisdropped)`,
	).Scan(&has); err != nil {
		return fmt.Errorf("read the ledger's columns: %w", err)
	}
	if !has {
		if _, err := a.conn.Exec(ctx,
			`ALTER TABLE w17_migrations ADD COLUMN IF NOT EXISTS post_tx_done INTEGER NOT NULL DEFAULT 0`,
			pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
	}
	a.cursorColEnsured = true
	return nil
}

// skirtFailure is the error of a skirt statement that failed: which one, what
// is already done, and the way out — a half-applied migration whose error
// named neither was the wedge the resume exists to prevent.
func (a *Applier) skirtFailure(ctx context.Context, m *applyfetchpb.Migration, stmts []string, i, from int, tracked bool, cause error) error {
	shown := strings.Join(strings.Fields(effectiveHead(stmts[i])), " ")
	if len(shown) > 120 {
		shown = shown[:120] + "…"
	}
	if !tracked {
		return fmt.Errorf("postgres apply up_post_tx: statement %d of %d (%s): %w\n"+
			"  migration %s is half-applied and has no resume point: its ledger row is written after its last statement, so the %d statement(s) before this one ran and are recorded nowhere, and a new deploy would run them again from the first (a re-run DROP or RENAME can drop a table that a later statement put in its place)\n"+
			"  fix: do NOT deploy it again unchanged — fix the cause, run statements %d–%d of its up_post_tx by hand, then record it with `migrate apply --fake` (which checks that the database holds what it describes); or undo the statements that ran, by hand",
			i+1, len(stmts), shown, cause, m.GetId(), i, i+1, len(stmts))
	}
	head, _ := a.AppliedHead(context.WithoutCancel(ctx))
	return fmt.Errorf("postgres apply up_post_tx: statement %d of %d (%s): %w\n"+
		"  migration %s is half-applied: its transaction committed and the %d statement(s) of its non-transactional part before this one ran and are recorded as done\n"+
		"  fix: correct the cause and deploy again — the apply resumes at this statement and never runs the ones before it again; or undo the migration with `migrate rollback --to %q` (it rolls back a half-applied migration too)",
		i+1, len(stmts), shown, cause, m.GetId(), i, head)
}
