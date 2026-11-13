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
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
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
		var known bool
		var err error
		if done, known, err = a.skirtDone(ctx, m.GetId()); err != nil {
			return fmt.Errorf("postgres post-tx resume point: %w", err)
		}
		if !known {
			// Mark the row as run by a cursor-aware applier before the first
			// statement: from here on its cursor is authoritative, which is
			// what lets a rollback skip the inverses of the statements it
			// never ran (skirtUndoPlan). NULL is a row an older applier ran
			// part of without recording anything.
			if err := setSkirtDone(ctx, a.conn, m.GetId(), 0); err != nil {
				return fmt.Errorf("postgres post-tx resume point: %w", err)
			}
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

// skirtDone reads a migration's cursor: how many of its skirt statements are
// recorded as done, and whether a cursor-aware applier recorded it at all
// (NULL: none did — an older applier may have run part of the skirt).
func (a *Applier) skirtDone(ctx context.Context, id string) (int, bool, error) {
	var n *int
	err := a.conn.QueryRow(ctx, `SELECT post_tx_done FROM w17_migrations WHERE to_char(timestamp AT TIME ZONE 'UTC', 'YYYYMMDD"T"HH24MISS"Z"') = $1`, id).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil || n == nil {
		return 0, false, err
	}
	return *n, true, nil
}

// ensureCursorColumn adds the `post_tx_done` column to the ledger when it is
// missing — the ledger's CREATE TABLE (a migration's own DDL) predates it.
// Nullable, no default: every row without it reads NULL, "no cursor-aware
// applier recorded this skirt" — what an older applier left, which may have
// run part of it. The resume treats NULL as 0 (from the top, as before); a
// rollback trusts only a recorded number (skirtUndoPlan). Asks the catalogue
// first, like ensurePhaseColumn, so a ledger that has it is never ALTERed.
func (a *Applier) ensureCursorColumn(ctx context.Context) error {
	if a.cursorColEnsured {
		return nil
	}
	has, err := a.hasCursorColumn(ctx)
	if err != nil {
		return err
	}
	if !has {
		if _, err := a.conn.Exec(ctx,
			`ALTER TABLE w17_migrations ADD COLUMN IF NOT EXISTS post_tx_done INTEGER`,
			pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
	}
	a.cursorColEnsured = true
	return nil
}

// hasCursorColumn reports from the catalogue, without touching the ledger,
// whether it carries `post_tx_done`.
func (a *Applier) hasCursorColumn(ctx context.Context) (bool, error) {
	if a.cursorColEnsured {
		return true, nil
	}
	var has bool
	if err := a.conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_attribute
		   WHERE attrelid = to_regclass('w17_migrations')
		     AND attname = 'post_tx_done' AND NOT attisdropped)`,
	).Scan(&has); err != nil {
		return false, fmt.Errorf("read the ledger's columns: %w", err)
	}
	return has, nil
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
		"  fix: correct the cause and deploy again — the apply resumes at this statement and never runs the ones before it again; or undo the migration with `migrate rollback --to %q` (it rolls back a half-applied migration too: it undoes the statements recorded as done, and skips the inverses of the ones that never ran when the migration marks its skirt operations — one minted before that runs every inverse, and says so if one fails)",
		i+1, len(stmts), shown, cause, m.GetId(), i, head)
}

// The rollback of a half-applied skirt (review R1-3).
//
// A skirt stopped at statement k; `migrate rollback` used to run the WHOLE
// down_pre_tx, so the inverse of an operation that never ran failed — an
// `ALTER TABLE "t" RENAME TO "t2"` while t2 still exists (42P07), a CREATE
// TABLE of a table never dropped, an ADD COLUMN of a column never dropped —
// and the documented way out stopped half-way too. The migrator marks each
// skirt operation in both directions (`-- w17:skirt-op N` heading its first
// statement in up_post_tx and its inverse in down_pre_tx; `-- w17:skirt-op
// always` for an inverse with no up of its own, and for the ledger erase).
// With the cursor, that says which operations ran.

// skirtOpRe reads an operation marker from a statement's leading comments.
var skirtOpRe = regexp.MustCompile(`(?m)^\s*--\s*w17:skirt-op\s+(\d+|always)\s*$`)

// skirtOpOf is the marker heading stmt ("" when none): its leading comment
// lines only, so a marker-shaped line inside a statement's body never counts.
func skirtOpOf(stmt string) string {
	lead := strings.TrimSpace(stmt)
	head := effectiveHead(stmt)
	lead = lead[:len(lead)-len(head)]
	if mm := skirtOpRe.FindStringSubmatch(lead); mm != nil {
		return mm[1]
	}
	return ""
}

// skirtUndoPlan decides which down_pre_tx statements a rollback runs: nil
// (all of them, as always) unless the migration is half-applied with a
// recorded cursor and marks its operations — then every inverse but those of
// the operations that never started. An operation started when one of its
// statements is recorded as done, or when the first unrecorded statement is
// its CONCURRENTLY one (a failed concurrent build leaves an INVALID index its
// inverse drops). pending reports a half-applied migration, for the message
// of an inverse that fails anyway.
func (a *Applier) skirtUndoPlan(ctx context.Context, m *applyfetchpb.Migration, down []string) (run []bool, pending bool, done int, err error) {
	if len(down) == 0 || m.GetUpPostTx() == "" {
		return nil, false, 0, nil
	}
	phase, err := a.MigrationPhase(ctx, m.GetId())
	if err != nil || phase != migrate.PhasePending {
		return nil, false, 0, err
	}
	has, err := a.hasCursorColumn(ctx)
	if err != nil || !has {
		return nil, true, 0, err
	}
	done, known, err := a.skirtDone(ctx, m.GetId())
	if err != nil || !known {
		return nil, true, 0, err
	}
	up := splitStatements(m.GetUpPostTx())
	starts := map[string]int{}
	var order []string
	for i, stmt := range up {
		if op := skirtOpOf(stmt); op != "" && op != "always" {
			starts[op] = i
			order = append(order, op)
		}
	}
	if len(order) == 0 {
		return nil, true, done, nil // minted before the markers: every inverse, as before
	}
	started := map[string]bool{}
	for j, op := range order {
		end := len(up)
		if j+1 < len(order) {
			end = starts[order[j+1]]
		}
		s := starts[op]
		started[op] = done > s || (done >= s && done < end && concurrentStmtRe.MatchString(effectiveHead(up[done])))
	}
	run = make([]bool, len(down))
	cur := "always"
	for i, stmt := range down {
		if op := skirtOpOf(stmt); op != "" {
			cur = op
		}
		_, marked := starts[cur]
		run[i] = cur == "always" || !marked || started[cur]
	}
	return run, true, done, nil
}

// rollbackPreTx runs down_pre_tx statement by statement, skipping what
// skirtUndoPlan says never ran.
func (a *Applier) rollbackPreTx(ctx context.Context, m *applyfetchpb.Migration) error {
	down := splitStatements(m.GetDownPreTx())
	run, pending, done, err := a.skirtUndoPlan(ctx, m, down)
	if err != nil {
		return fmt.Errorf("postgres rollback down_pre_tx: read the half-applied migration's resume point: %w", err)
	}
	for i, stmt := range down {
		if run != nil && !run[i] {
			continue
		}
		if _, err := a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
			if !pending || run != nil {
				return fmt.Errorf("postgres rollback down_pre_tx: %w", err)
			}
			return fmt.Errorf("postgres rollback down_pre_tx: statement %d of %d: %w\n"+
				"  migration %s is half-applied (%d statement(s) of its non-transactional part recorded as done), and it does not mark its operations (or an older applier ran its skirt without recording how far it got), so this rollback runs EVERY inverse in down_pre_tx — including those of statements that never ran, which is what fails here when this one undoes such a statement\n"+
				"  fix: undo by hand the statements of its up_post_tx that ran (the first %d, and the in-transaction part), then delete its row from w17_migrations; a migration minted by a newer console skips the inverses of the statements that never ran",
				i+1, len(down), err, m.GetId(), done, done)
		}
	}
	return nil
}
