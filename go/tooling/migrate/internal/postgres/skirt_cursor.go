package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// The skirt's resume cursor.
//
// A skirt runs one statement at a time after the transaction's COMMIT, and a
// deploy can stop between any two of them: a statement fails on the data (a
// SET NOT NULL over a NULL, a unique index over duplicates — the "fix the rows
// and deploy again" flow), or the process dies. The resume used to run the
// skirt again from its FIRST statement. That was safe while a skirt held only
// a concurrent index's `DROP INDEX CONCURRENTLY IF EXISTS` + `CREATE INDEX
// CONCURRENTLY`; it is not since a skirt carries the DDL coupled to such an
// index: a re-run DROP COLUMN fails 42703 forever, and the `DROP TABLE IF
// EXISTS "t"` ahead of `ALTER TABLE "t2" RENAME TO "t"` drops the renamed
// table — rows and all — on the ordinary retry.
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
//
// A NULL cursor under a pending row. An applier that records no cursor —
// sdk af86452, released with the skirt flood — runs the skirt from the top
// and records nothing, so a deploy of it that stopped part-way leaves a
// pending row whose cursor is NULL with statements already run. Taken as 0,
// the next deploy ran them again and the `DROP TABLE IF EXISTS` ahead of a
// RENAME dropped the renamed table with its rows. So:
//
//   - the console heads every skirt with a WITNESS (`-- w17:skirt-witness`
//     over a DO block that raises): such an applier runs it first and stops
//     there, before any DDL of the skirt, saying to upgrade; this one records
//     it done without running it. Under the witness, NULL means nothing ran;
//   - a resume over NULL on a skirt minted without the witness refuses,
//     lists the statements and names the ways out (record how many ran and
//     deploy again, or roll back), and writes nothing — the rollback must
//     still see NULL and run every inverse;
//   - NULL right after this call's own in-tx half (Apply) is 0: nothing ran;
//   - a ledger this role cannot ALTER to add the column refuses the skirt,
//     which is never run untracked — before the in-tx half when Apply can
//     tell.

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
// cursor (the caller re-checked the phase under the skirt's lock). resume:
// the caller is ApplyPostTx — the pending row was NOT written by this call.
func (a *Applier) runSkirt(ctx context.Context, m *applyfetchpb.Migration, stmts []string, tracked, resume bool) error {
	done := 0
	if tracked {
		if err := a.ensureCursorColumn(ctx); err != nil {
			// Never run a skirt untracked under a pending row. It used to
			// warn and run from the top, and the next deploy ran it from the
			// top again — a re-run DROP TABLE IF EXISTS ahead of a RENAME
			// drops the renamed table.
			return a.cursorPrivilegeRefusal(ctx, m, err, true)
		}
		// A rollback of this migration that stopped part-way undid some of
		// it: running it forward now would apply the rest over a half-undone
		// schema, and a later rollback would skip inverses on a stale record.
		if started, undone, err := a.undoneSoFar(ctx, m.GetId()); err != nil {
			return fmt.Errorf("postgres post-tx resume point: %w", err)
		} else if started {
			return fmt.Errorf("postgres apply up_post_tx: a rollback of migration %s stopped part-way — %d statement(s) of its down_pre_tx are recorded as done — so it is not applied forward over the half-undone schema\n"+
				"  fix: finish the rollback (`migrate rollback`, which resumes where it stopped), then deploy again; or, when you have checked that none of its inverses changed the database, abandon it — `UPDATE w17_migrations SET post_tx_undone = NULL WHERE timestamp = '%s';` — and deploy again", m.GetId(), undone, ledgerTimestamp(m.GetId()))
		}
		var known bool
		var err error
		if done, known, err = a.skirtDone(ctx, m.GetId()); err != nil {
			return fmt.Errorf("postgres post-tx resume point: %w", err)
		}
		if !known {
			// NULL: no cursor-aware applier recorded this skirt. Nothing of it
			// ran when this very call just committed the in-tx half (!resume),
			// or when the skirt is headed by the witness — an applier that
			// records no cursor stops AT the witness, before any statement of
			// the skirt runs. Otherwise an older applier may have run part of
			// it, and from the top would run that part again (the renamed
			// table dropped with its rows): refuse, and write nothing — a
			// rollback must still see NULL and run every inverse.
			if resume && !carriesWitness(stmts) {
				return a.unrecordedResumeRefusal(ctx, m, stmts)
			}
			// Mark the row as run by a cursor-aware applier before the first
			// statement: from here on its cursor is authoritative, which is
			// what lets a rollback skip the inverses of the statements it
			// never ran (skirtUndoPlan).
			if err := setSkirtDone(ctx, a.conn, m.GetId(), 0); err != nil {
				return fmt.Errorf("postgres post-tx resume point: %w", err)
			}
		}
		if done > len(stmts) {
			return fmt.Errorf("postgres apply up_post_tx: the ledger records %d statements of migration %s's non-transactional part as done, and it has %d — this is not the migration that ran here", done, m.GetId(), len(stmts))
		}
	}
	recorded := done
	resumeAt := -1
	if tracked && resume {
		resumeAt = done
	}
	for i := done; i < len(stmts); i++ {
		stmt := stmts[i]
		head := effectiveHead(stmt)
		var err error
		switch {
		case isSkirtWitness(stmt):
			// The witness stops an applier that records no cursor; this one
			// records it done without running it.
			if tracked {
				err = setSkirtDone(ctx, a.conn, m.GetId(), i+1)
			}
		case !tracked:
			_, err = a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol)
		case concurrentStmtRe.MatchString(head):
			advance := !rerunSafeDropRe.MatchString(head)
			if i == resumeAt {
				var skip bool
				if skip, err = a.settleInterruptedBuild(ctx, head); err != nil {
					return a.skirtFailure(ctx, m, stmts, i, recorded, tracked, err)
				}
				if skip {
					if err = setSkirtDone(ctx, a.conn, m.GetId(), i+1); err != nil {
						return a.skirtFailure(ctx, m, stmts, i, recorded, tracked, err)
					}
					recorded = i + 1
					continue
				}
			}
			err = a.runAlone(ctx, m.GetId(), stmt, i+1, advance)
			if err == nil && advance {
				recorded = i + 1
			}
			if err != nil {
				return a.skirtFailure(ctx, m, stmts, i, recorded, tracked, err)
			}
			continue
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
			return a.skirtFailure(ctx, m, stmts, i, recorded, tracked, err)
		}
		if tracked {
			recorded = i + 1
		}
	}
	return nil
}

// skirtWitnessRe reads the witness marker from a statement's leading
// comments (emit/postgres RenderW17MigrationsSkirtWitness).
var skirtWitnessRe = regexp.MustCompile(`(?m)^\s*--\s*w17:skirt-witness\s*$`)

// isSkirtWitness reports whether stmt is the witness the console heads a
// skirt with: a DO block that raises, under its marker. An applier that
// records no resume cursor runs it and stops there, before any statement of
// the skirt; this one records it done instead.
func isSkirtWitness(stmt string) bool {
	head := effectiveHead(stmt)
	lead := strings.TrimSpace(stmt)
	lead = lead[:len(lead)-len(head)]
	return skirtWitnessRe.MatchString(lead) && strings.HasPrefix(head, "DO $w17_skirt$")
}

// carriesWitness reports whether a skirt is headed by the witness — minted by
// a console that writes one. A NULL cursor on such a skirt means nothing of
// it ran.
func carriesWitness(stmts []string) bool {
	return len(stmts) > 0 && isSkirtWitness(stmts[0])
}

// ledgerTimestamp renders a migration id (`20260201T000000Z`) as the ledger's
// timestamp literal, for a statement an operator runs by hand.
func ledgerTimestamp(id string) string {
	if t, err := time.Parse("20060102T150405Z", id); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05Z")
	}
	return id
}

// unrecordedResumeRefusal is the refusal of a resume over a pending row whose
// cursor no applier recorded, on a skirt minted without the witness:
// which statements there are, and the two ways out — record how many ran and
// deploy again, or roll back (every inverse, as nothing is recorded).
func (a *Applier) unrecordedResumeRefusal(ctx context.Context, m *applyfetchpb.Migration, stmts []string) error {
	var list strings.Builder
	for i, s := range stmts {
		fmt.Fprintf(&list, "    statement %d: %s\n", i+1, shownStatement(s))
	}
	head, _ := a.AppliedHead(context.WithoutCancel(ctx))
	return fmt.Errorf("postgres apply up_post_tx: migration %s is half-applied and its ledger row records no resume point (post_tx_done is NULL), so this applier cannot tell how many of the %d statement(s) of its non-transactional part already ran\n"+
		"  why: an applier that does not record one (sdk/go before the skirt resume cursor) ran part of it, or a deploy stopped right after its transaction committed; running it again from the first statement can drop a table a later statement renamed into place, rows and all, so this deploy runs none of it\n"+
		"  its statements:\n%s"+
		"  fix: compare the database with these statements and record how many of them ran — `UPDATE w17_migrations SET post_tx_done = <k> WHERE timestamp = '%s';` (k = 0 when none did) — correct what stopped it, and deploy again: the apply resumes at statement k+1. Or undo it with `migrate rollback --to %q`: with no recorded resume point the rollback runs every inverse, and says so if one fails",
		m.GetId(), len(stmts), list.String(), ledgerTimestamp(m.GetId()), head)
}

// cursorPrivilegeRefusal is the refusal of a skirt the applier cannot track
// because it may not ALTER the ledger to add the cursor column: a skirt is
// never run untracked. pending: the in-tx half committed (a resume, or this
// call's own apply); otherwise nothing of the migration ran.
func (a *Applier) cursorPrivilegeRefusal(ctx context.Context, m *applyfetchpb.Migration, cause error, pending bool) error {
	role := "<this role>"
	var cu string
	if err := a.conn.QueryRow(context.WithoutCancel(ctx), `SELECT current_user`).Scan(&cu); err == nil && cu != "" {
		role = pgx.Identifier{cu}.Sanitize()
	}
	state := "nothing of this migration ran"
	if pending {
		state = "its transaction committed and this deploy ran none of its non-transactional part"
	}
	return fmt.Errorf("postgres apply up_post_tx: migration %s: the ledger w17_migrations has no post_tx_done column and this role cannot add it: %w\n"+
		"  why: a migration's non-transactional part runs one statement at a time, and post_tx_done records how many ran so a deploy that stops part-way resumes after them; run without it, the next deploy would run it from the first statement again, and a re-run DROP TABLE ahead of a RENAME drops the renamed table with its rows — so it is not run untracked (%s)\n"+
		"  fix: adding the column needs ownership of the ledger: make this role its owner (`ALTER TABLE w17_migrations OWNER TO %s;`), or have its owner run `ALTER TABLE w17_migrations ADD COLUMN IF NOT EXISTS post_tx_done INTEGER;` once; then deploy again",
		m.GetId(), cause, state, role)
}

// shownStatement is a statement on one line, cut at 120 characters.
func shownStatement(stmt string) string {
	shown := strings.Join(strings.Fields(effectiveHead(stmt)), " ")
	if len(shown) > 120 {
		shown = shown[:120] + "…"
	}
	return shown
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

// createIndexConcurrentlyRe reads the index name and its table off a
// CREATE [UNIQUE] INDEX CONCURRENTLY statement (IF NOT EXISTS needs nothing:
// it re-runs cleanly). Names are taken as written — bare, quoted or
// schema-qualified: group 1 is the index, group 2 its schema part when it has
// one, group 3 the table after ON [ONLY].
var createIndexConcurrentlyRe = regexp.MustCompile(`(?is)^CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+(` + pgIdent + `(\.` + pgIdent + `)?)\s+ON\s+(?:ONLY\s+)?(` + pgIdent + `(?:\.` + pgIdent + `)?)`)

// pgIdent is one identifier as a statement spells it: quoted, or bare.
const pgIdent = `(?:"(?:[^"]|"")+"|[^\s"(.]+)`

// interruptedIndexQuery finds the index an interrupted build left. An index
// is created in its TABLE's schema, not the search_path's, so a bare index
// name is resolved in the schema of the table it was built on ($2) — w17's
// own tables are schema-qualified, and to_regclass of the bare name would
// miss every one of them. It answers the index's validity and its name as a
// DROP must spell it.
const interruptedIndexQuery = `SELECT i.indisvalid, i.indexrelid::regclass::text FROM pg_index i
WHERE i.indexrelid = to_regclass(CASE WHEN $3 THEN $1 ELSE COALESCE(
  (SELECT quote_ident(n.nspname) || '.' FROM pg_class t JOIN pg_namespace n ON n.oid = t.relnamespace WHERE t.oid = to_regclass($2)), '') || $1 END)`

// settleInterruptedBuild decides the CONCURRENTLY build a resume starts at.
// A build autocommits and its cursor is recorded after it, so a process that
// died between the two left the cursor here with the build done — or, when it
// died during the build, left an INVALID index. Either way the statement can
// not simply run again: CREATE fails "already exists" on every deploy. A
// valid index of that name is the build that finished: skip (true). An
// invalid one is a build that did not: drop it, then run the statement.
// A build paired with a DROP … IF EXISTS before it never starts a resume
// (the drop is not recorded), and other CONCURRENTLY statements are run as
// they are.
func (a *Applier) settleInterruptedBuild(ctx context.Context, head string) (bool, error) {
	mm := createIndexConcurrentlyRe.FindStringSubmatch(head)
	if mm == nil {
		return false, nil
	}
	var valid *bool
	var name string
	err := a.conn.QueryRow(ctx, interruptedIndexQuery, mm[1], mm[3], mm[2] != "").Scan(&valid, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the index this resume starts at (%s): %w", mm[1], err)
	}
	if valid != nil && *valid {
		return true, nil
	}
	if _, err := a.conn.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+name, pgx.QueryExecModeSimpleProtocol); err != nil {
		return false, fmt.Errorf("drop the INVALID index an interrupted build left (%s): %w", name, err)
	}
	return false, nil
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
// run part of it. A resume over NULL runs from the top only under the
// witness, and refuses otherwise (runSkirt); a rollback trusts only a
// recorded number, or the witness (skirtUndoPlan). Asks the catalogue first,
// like ensurePhaseColumn, so a ledger that has it is never ALTERed.
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
// named neither was the wedge the resume exists to prevent. recorded is the
// cursor as the ledger holds it now: a `DROP INDEX CONCURRENTLY IF EXISTS`
// ahead of a concurrent build is not recorded on its own (it re-runs with
// the build), so it can be less than i.
func (a *Applier) skirtFailure(ctx context.Context, m *applyfetchpb.Migration, stmts []string, i, recorded int, tracked bool, cause error) error {
	shown := shownStatement(stmts[i])
	n := len(stmts)
	if !tracked {
		// The legacy layout: an all-post-tx migration minted before it got a
		// pending row (an untracked run under one is refused). Its LAST
		// statement is the ledger INSERT that records it, so running the
		// rest by hand includes that statement, and `--fake` has nothing
		// left to record (its INSERT meets the row: 23505).
		finish := fmt.Sprintf("run statement %d of its up_post_tx by hand — it is the ledger INSERT that records the migration", n)
		if i+1 < n {
			finish = fmt.Sprintf("run statements %d–%d of its up_post_tx by hand, then statement %d, the ledger INSERT that records the migration", i+1, n-1, n)
		}
		return fmt.Errorf("postgres apply up_post_tx: statement %d of %d (%s): %w\n"+
			"  migration %s is half-applied and has no resume point: its ledger row is written by its last statement (%d), so the %d statement(s) before this one ran and are recorded nowhere, and a new deploy would run them again from the first (a re-run DROP or RENAME can drop a table that a later statement put in its place)\n"+
			"  fix: do NOT deploy it again unchanged — fix the cause, %s; or undo the statements that ran, by hand",
			i+1, n, shown, cause, m.GetId(), n, i, finish)
	}
	head, _ := a.AppliedHead(context.WithoutCancel(ctx))
	again := ""
	if recorded < i {
		again = fmt.Sprintf(" (statements %d–%d ran too and are not recorded: a DROP INDEX … IF EXISTS ahead of a concurrent build, which the resume runs again with the build)", recorded+1, i)
	}
	return fmt.Errorf("postgres apply up_post_tx: statement %d of %d (%s): %w\n"+
		"  migration %s is half-applied: its transaction committed and the first %d statement(s) of its non-transactional part are recorded as done%s\n"+
		"  fix: correct the cause and deploy again — the apply resumes at statement %d and never runs the recorded ones again; or undo the migration with `migrate rollback --to %q` (it rolls back a half-applied migration too: it undoes the statements recorded as done, and skips the inverses of the ones that never ran when the migration marks its skirt operations — one minted before that runs every inverse, and says so if one fails)",
		i+1, n, shown, cause, m.GetId(), recorded, again, recorded+1, head)
}

// The rollback of a half-applied skirt.
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
//
// An operation can itself be several statements, and stop part-way: a
// DropTable is `DROP INDEX …; DROP TABLE …;`, and when the DROP TABLE fails
// (a dependent view) the index is gone and the table is not. Its inverse
// (`CREATE TABLE …; CREATE INDEX …;`) run whole failed on the CREATE TABLE —
// 42P07 on every retry. So an inverse statement may also name the UP
// statement of its operation it undoes, `-- w17:skirt-op N.k` (k = the
// offset of that statement inside operation N; UP and DOWN do not pair one
// to one — a DropTable's inverse re-creates the table before its indexes),
// and runs only when that statement ran; the statements after it, up to the
// next marker, undo the same one. An operation whose inverse names none runs
// whole whenever it started, as before. An applier that does not
// know the form reads the plain `N` line ahead of it and ignores it.

// skirtOpRe reads an operation marker from a statement's leading comments:
// the operation, and the UP statement inside it the statement undoes.
var skirtOpRe = regexp.MustCompile(`(?m)^\s*--\s*w17:skirt-op\s+(\d+|always)(?:\.(\d+))?\s*$`)

// skirtMark is the marker heading one statement: its operation ("" when none,
// "always"), and the offset of the UP statement it undoes (k < 0: none named).
type skirtMark struct {
	op string
	k  int
}

// skirtOpOf is the marker heading stmt: its leading comment lines only, so a
// marker-shaped line inside a statement's body never counts. The first line
// names the operation; an `N.k` line names the statement it undoes.
func skirtOpOf(stmt string) skirtMark {
	lead := strings.TrimSpace(stmt)
	head := effectiveHead(stmt)
	lead = lead[:len(lead)-len(head)]
	mk := skirtMark{k: -1}
	for _, mm := range skirtOpRe.FindAllStringSubmatch(lead, -1) {
		if mk.op == "" {
			mk.op = mm[1]
		}
		if mm[2] != "" && mm[1] == mk.op {
			if k, err := strconv.Atoi(mm[2]); err == nil {
				mk.k = k
			}
		}
	}
	return mk
}

// skirtUndoMark advances the operation and named UP statement a down_pre_tx
// statement belongs to: a marker of another operation starts it (with the
// statement it names, or none), an `N.k` line names the statement, and an
// unmarked statement keeps both — a statement named once covers the ones
// after it up to the next marker.
func skirtUndoMark(stmt, cur string, k int) (string, int) {
	mk := skirtOpOf(stmt)
	if mk.op != "" && mk.op != cur {
		return mk.op, mk.k
	}
	if mk.k >= 0 {
		return cur, mk.k
	}
	return cur, k
}

// skirtUndoPlan decides which down_pre_tx statements a rollback runs: nil
// (all of them, as always) unless the migration is half-applied with a
// recorded cursor and marks its operations — then every inverse but those of
// the operations that never started, and, inside a started operation, but
// those that name an UP statement that never ran. An UP statement ran when
// the cursor records it, or when the cursor stops at a run of CONCURRENTLY
// statements that reaches it (a failed concurrent build leaves an INVALID
// index its inverse drops; the `DROP … IF EXISTS` ahead of it is not recorded
// on its own). pending reports a half-applied migration, for the message of
// an inverse that fails anyway; ops lists, per down_pre_tx statement, the
// operation it belongs to.
func (a *Applier) skirtUndoPlan(ctx context.Context, m *applyfetchpb.Migration, down []string) (run []bool, pending bool, done int, err error) {
	if len(down) == 0 || m.GetUpPostTx() == "" {
		return nil, false, 0, nil
	}
	phase, err := a.MigrationPhase(ctx, m.GetId())
	if err != nil || phase != migrate.PhasePending {
		return nil, false, 0, err
	}
	// A complete migration whose rollback stopped part-way reads as pending
	// (appliedRowExpr), and every one of its inverses is due.
	if complete, err := a.rowComplete(ctx, m.GetId()); err != nil || complete {
		return nil, false, 0, err
	}
	up := splitStatements(m.GetUpPostTx())
	// Under the witness an unrecorded cursor — no column, or NULL — means
	// nothing of the skirt ran: an applier that records none stops at the
	// witness. Without it, it may mean anything, and every inverse runs.
	witness := carriesWitness(up)
	has, err := a.hasCursorColumn(ctx)
	if err != nil {
		return nil, true, 0, err
	}
	known := false
	if has {
		if done, known, err = a.skirtDone(ctx, m.GetId()); err != nil {
			return nil, true, 0, err
		}
	}
	if !known {
		if !witness {
			return nil, true, 0, nil
		}
		done = 0
	}
	starts := map[string]int{}
	var order []string
	for i, stmt := range up {
		if op := skirtOpOf(stmt).op; op != "" && op != "always" {
			starts[op] = i
			order = append(order, op)
		}
	}
	if len(order) == 0 {
		return nil, true, done, nil // minted before the markers: every inverse, as before
	}
	// ran reports whether UP statement j ran (or may have left something an
	// inverse must undo).
	ran := func(j int) bool {
		if done > j || j >= len(up) {
			return true
		}
		for i := done; i <= j; i++ {
			if !concurrentStmtRe.MatchString(effectiveHead(up[i])) {
				return false
			}
		}
		return true
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
	cur, k := "always", -1
	for i, stmt := range down {
		cur, k = skirtUndoMark(stmt, cur, k)
		s, marked := starts[cur]
		switch {
		case cur == "always" || !marked:
			run[i] = true
		case !started[cur]:
			run[i] = false
		case k >= 0:
			run[i] = ran(s + k)
		default:
			run[i] = true
		}
	}
	return run, true, done, nil
}

// rowComplete reports whether the migration's ledger row records its post-tx
// half complete (a ledger without the column holds only complete rows).
func (a *Applier) rowComplete(ctx context.Context, id string) (bool, error) {
	has, err := a.hasPhaseColumn(ctx)
	if err != nil || !has {
		return true, err
	}
	var complete bool
	err = a.conn.QueryRow(ctx, `SELECT post_tx_complete FROM w17_migrations WHERE `+skirtRowWhere1, id).Scan(&complete)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return complete, err
}

// rollbackPreTx runs down_pre_tx statement by statement, skipping what
// skirtUndoPlan says never ran, from the first statement a stopped rollback
// did not record as done: an inverse that ran — a RENAME back, a re-added
// column — cannot run twice, and the rollback that stopped on the data used
// to run every one again and wedge on the first.
//
// A CONCURRENTLY inverse is resumed the way the forward skirt resumes one: a
// `DROP INDEX CONCURRENTLY IF EXISTS` is not recorded on its own, so a resume
// runs it again with the build after it (an INVALID leftover of a failed
// build is dropped, never met by `already exists`), and a build the resume
// starts at (the first statement it runs) is settled first — skipped when a
// valid index of its name stands (it finished and the record did not), its
// INVALID leftover dropped.
func (a *Applier) rollbackPreTx(ctx context.Context, m *applyfetchpb.Migration) error {
	down := splitStatements(m.GetDownPreTx())
	if len(down) == 0 {
		return nil
	}
	run, pending, done, err := a.skirtUndoPlan(ctx, m, down)
	if err != nil {
		return fmt.Errorf("postgres rollback down_pre_tx: read the half-applied migration's resume point: %w", err)
	}
	tracked, from, resumed, untracked := a.undoStart(ctx, m.GetId())
	if from > len(down) {
		return fmt.Errorf("postgres rollback down_pre_tx: the ledger records %d statements of migration %s's down_pre_tx as done, and it has %d — this is not the migration that ran here", from, m.GetId(), len(down))
	}
	recorded := from
	first := true
	for i := from; i < len(down); i++ {
		if run != nil && !run[i] {
			continue
		}
		settle := first
		first = false
		stmt := down[i]
		if settle && tracked && !resumed {
			// The row says a rollback is under way before its first inverse
			// runs: a CONCURRENTLY drop runs unrecorded, and a rollback that
			// stops right after one has changed the schema with nothing
			// counted. From here the ledger's readers do not call the
			// migration applied (appliedRowExpr) and the forward resume
			// refuses (runSkirt).
			if err := setUndone(ctx, a.conn, m.GetId(), from); err != nil {
				return fmt.Errorf("postgres rollback down_pre_tx: record the rollback's start: %w", err)
			}
		}
		head := effectiveHead(stmt)
		var err error
		switch {
		case !tracked:
			_, err = a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol)
		case concurrentStmtRe.MatchString(head):
			advance := !rerunSafeDropRe.MatchString(head)
			if settle {
				var skip bool
				if skip, err = a.settleInterruptedBuild(ctx, head); err == nil && skip {
					err = setUndone(ctx, a.conn, m.GetId(), i+1)
					if err == nil {
						recorded = i + 1
						continue
					}
				}
				if err != nil {
					break
				}
			}
			err = a.runAloneUndo(ctx, m.GetId(), stmt, i+1, advance)
			if err == nil && advance {
				recorded = i + 1
			}
		default:
			err = a.runWithUndo(ctx, m.GetId(), stmt, i+1)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "25001" {
				err = a.runAloneUndo(ctx, m.GetId(), stmt, i+1, true)
			}
			if err == nil {
				recorded = i + 1
			}
		}
		if err == nil {
			continue
		}
		if pending && run == nil {
			return fmt.Errorf("postgres rollback down_pre_tx: statement %d of %d: %w\n"+
				"  migration %s is half-applied (%d statement(s) of its non-transactional part recorded as done), and it does not mark its operations (or an older applier ran its skirt without recording how far it got), so this rollback runs EVERY inverse in down_pre_tx — including those of statements that never ran, which is what fails here when this one undoes such a statement\n"+
				"  fix: undo by hand the statements of its up_post_tx that ran (the first %d, and the in-transaction part), then delete its row from w17_migrations; a migration minted by a newer console skips the inverses of the statements that never ran",
				i+1, len(down), err, m.GetId(), done, done)
		}
		if !tracked {
			return fmt.Errorf("postgres rollback down_pre_tx: statement %d of %d: %w\n"+
				"  this rollback records no resume point (%s), so the %d statement(s) of its down_pre_tx before this one ran and a new rollback runs them again from the first\n"+
				"  fix: correct the cause, then run statements %d–%d of its down_pre_tx and its down_sql by hand",
				i+1, len(down), err, untracked, i, i+1, len(down))
		}
		if pending && run != nil {
			if why := a.halfRunOpUndo(m, down, i, done); why != "" {
				return fmt.Errorf("postgres rollback down_pre_tx: statement %d of %d: %w\n%s", i+1, len(down), err, why)
			}
		}
		again := ""
		if recorded < i {
			again = fmt.Sprintf("; statements %d–%d ran and are not recorded (a DROP INDEX CONCURRENTLY IF EXISTS ahead of a concurrent build, which the resume runs again with the build)", recorded+1, i)
		}
		return fmt.Errorf("postgres rollback down_pre_tx: statement %d of %d: %w\n"+
			"  the rollback of migration %s stopped part-way: the first %d statement(s) of its down_pre_tx are recorded as done (or skipped as inverses of statements that never ran)%s\n"+
			"  fix: correct the cause and roll back again — the rollback resumes at statement %d and never runs the recorded ones again",
			i+1, len(down), err, m.GetId(), recorded, again, recorded+1)
	}
	return nil
}

// halfRunOpUndo is the explanation of a failed inverse that belongs to an
// operation the skirt stopped inside (some of its statements recorded, not
// all) when the migration does not name, per inverse statement, the UP
// statement it undoes (no `-- w17:skirt-op N.k` line — a console that
// predates the lines, or a renderer that writes none): the rollback runs that
// operation's whole inverse, and the failing statement may undo one that
// never ran. "" when that is not the case. The message does not say why the
// names are missing: the applier cannot tell an old migration from a gap in
// the renderer.
func (a *Applier) halfRunOpUndo(m *applyfetchpb.Migration, down []string, i, done int) string {
	up := splitStatements(m.GetUpPostTx())
	cur, k := "always", -1
	for j := 0; j <= i; j++ {
		cur, k = skirtUndoMark(down[j], cur, k)
	}
	if cur == "always" || k >= 0 {
		return ""
	}
	s, end := -1, len(up)
	for j, stmt := range up {
		op := skirtOpOf(stmt).op
		if op == cur {
			s = j
		} else if s >= 0 && op != "" {
			end = j
			break
		}
	}
	if end == len(up) && completesAPendingRow(up) {
		end-- // the completing UPDATE belongs to no operation
	}
	if s < 0 || done <= s || done >= end {
		return ""
	}
	return fmt.Sprintf("  operation %s of migration %s's non-transactional part stopped after %d of its %d statement(s), and the migration does not name which UP statement each of the operation's inverse statements undoes, so this rollback runs the operation's whole inverse — this statement may undo one that never ran\n"+
		"  fix: if what this statement re-creates or restores is still in place, skip it — `UPDATE w17_migrations SET post_tx_undone = %d WHERE timestamp = '%s';` — and roll back again; otherwise correct the cause and roll back again (the rollback resumes at statement %d)",
		cur, m.GetId(), done-s, end-s, i+1, ledgerTimestamp(m.GetId()), i+1)
}

// undoStart reads where a rollback of the migration's down_pre_tx starts: the
// statement after the last one a stopped rollback recorded (post_tx_undone;
// NULL is 0, and no rollback has started — resumed false). The column is
// added when missing; a ledger this role cannot ALTER, or a migration with no
// ledger row, rolls back untracked as before, and why is the last value.
func (a *Applier) undoStart(ctx context.Context, id string) (tracked bool, from int, resumed bool, untracked string) {
	if err := a.ensureUndoColumn(ctx); err != nil {
		return false, 0, false, "the ledger has no post_tx_undone column and this role cannot add it: " + err.Error()
	}
	var n *int
	err := a.conn.QueryRow(ctx, `SELECT post_tx_undone FROM w17_migrations WHERE `+skirtRowWhere1, id).Scan(&n)
	if err != nil {
		return false, 0, false, "the migration's ledger row could not be read: " + err.Error()
	}
	if n == nil {
		return true, 0, false, ""
	}
	return true, *n, true, ""
}

// skirtRowWhere1 is skirtRowWhere with the id as the only parameter.
const skirtRowWhere1 = `to_char(timestamp AT TIME ZONE 'UTC', 'YYYYMMDD"T"HH24MISS"Z"') = $1`

// runWithUndo runs one inverse and records it done, in one transaction. The
// record goes first: the inverse may be the ledger erase, or the ledger's
// DROP TABLE, after which there is no row to record it on.
func (a *Applier) runWithUndo(ctx context.Context, id, stmt string, n int) error {
	tx, err := a.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := setUndone(ctx, tx, id, n); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// runAloneUndo runs one inverse PostgreSQL refuses in a transaction, then
// (advance) records it done.
func (a *Applier) runAloneUndo(ctx context.Context, id, stmt string, n int, advance bool) error {
	if _, err := a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if !advance {
		return nil
	}
	return setUndone(ctx, a.conn, id, n)
}

func setUndone(ctx context.Context, e execer, id string, n int) error {
	_, err := e.Exec(ctx, `UPDATE w17_migrations SET post_tx_undone = $1 WHERE `+skirtRowWhere, n, id)
	return err
}

// ensureUndoColumn adds the rollback's resume cursor (`post_tx_undone`,
// nullable: NULL is "no rollback of this migration has run") when the ledger
// lacks it, asking the catalogue first like ensureCursorColumn.
func (a *Applier) ensureUndoColumn(ctx context.Context) error {
	if a.undoColEnsured {
		return nil
	}
	has, err := a.hasLedgerColumn(ctx, "post_tx_undone")
	if err != nil {
		return err
	}
	if !has {
		if _, err := a.conn.Exec(ctx,
			`ALTER TABLE w17_migrations ADD COLUMN IF NOT EXISTS post_tx_undone INTEGER`,
			pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
	}
	a.undoColEnsured = true
	return nil
}

// hasLedgerColumn reports from the catalogue whether the ledger carries col.
func (a *Applier) hasLedgerColumn(ctx context.Context, col string) (bool, error) {
	var has bool
	if err := a.conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_attribute
		   WHERE attrelid = to_regclass('w17_migrations')
		     AND attname = $1 AND NOT attisdropped)`, col,
	).Scan(&has); err != nil {
		return false, fmt.Errorf("read the ledger's columns: %w", err)
	}
	return has, nil
}

// undoneSoFar reads whether a rollback of the migration started and stopped
// (post_tx_undone not NULL) and how many down_pre_tx statements it recorded
// (0 also when only an unrecorded CONCURRENTLY drop ran). No row, or a ledger
// without the column: none started.
func (a *Applier) undoneSoFar(ctx context.Context, id string) (bool, int, error) {
	has, err := a.hasLedgerColumn(ctx, "post_tx_undone")
	if err != nil || !has {
		return false, 0, err
	}
	var n *int
	err = a.conn.QueryRow(ctx, `SELECT post_tx_undone FROM w17_migrations WHERE `+skirtRowWhere1, id).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) || n == nil {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, *n, nil
}
