// Package postgres is the production-side PostgreSQL Applier
// for w17ctl. Connects via pgx (`github.com/jackc/pgx/v5`)
// and uses the simple-query protocol so multi-statement DDL
// (BEGIN; … COMMIT; bodies the migrator emits) executes in a
// single Exec call without per-statement splitting.
//
// The migrator emits two SQL bodies per migration:
//
//   - up_sql carries the transactional half. It already contains
//     its own BEGIN / COMMIT framing — we just stream it.
//   - up_post_tx carries non-transactional ops (CREATE INDEX
//     CONCURRENTLY etc.) that PG refuses inside a transaction.
//     Run only when the migration declares one.
//
// On apply error: the error surfaces verbatim. PG's pgconn.PgError
// already carries detail / hint / position; we wrap with a short
// prefix so logs cluster by phase but otherwise pass through.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Applier is the per-connection PG apply driver. Construct via
// New (Connect-then-cache). One Applier per consuming-service
// connection (one per `--target` flag); shared across every
// pending migration for that connection within a single
// w17ctl migrate apply run.
type Applier struct {
	conn *pgx.Conn
	dsn  string

	// phaseColEnsured caches the idempotent Q52 bootstrap (ALTER TABLE
	// … ADD COLUMN IF NOT EXISTS post_tx_complete) so it runs at most
	// once per connection. See ensurePhaseColumn.
	phaseColEnsured bool
}

// Compile-time check the impl satisfies the migrate.Applier
// contract + the optional FingerprintCapable for Phase D drift
// detection + the optional ResumableApplier for Q52 two-phase
// crash recovery.
var _ migrate.Applier = (*Applier)(nil)
var _ migrate.Wiper = (*Applier)(nil)
var _ migrate.FingerprintCapable = (*Applier)(nil)
var _ migrate.ResumableApplier = (*Applier)(nil)

// New opens a pgx connection to the supplied DSN. Accepts both
// keyword (`host=… user=… …`) and URI (`postgres://…`) forms;
// see pgx documentation for the full grammar.
//
// The connection is opened eagerly so DSN typos surface at New
// time rather than on the first Apply, simplifying deploy
// debugging.
func New(ctx context.Context, dsn string) (*Applier, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres.New: dsn is empty")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres.New: %w", err)
	}
	return &Applier{conn: conn, dsn: dsn}, nil
}

// AppliedHead returns the id of the most recently applied
// migration on this DB by querying `wc_migrations` (D27).
// Missing table = empty string (treated as fresh DB by the
// orchestrator).
//
// `wc_migrations.timestamp` is TIMESTAMPTZ; the orchestrator's
// pending filter compares against `naming.Name` strings
// (`YYYYMMDDTHHMMSSZ` basic ISO-8601). We format here via
// `to_char` so the returned string round-trips against the id
// the registry stamped on each Migration.
func (a *Applier) AppliedHead(ctx context.Context) (string, error) {
	// Q52: bootstrap the post_tx_complete column on pre-existing
	// trackers before the filtered query, then exclude incomplete
	// (post_tx_complete=false) rows from the head. A partially-applied
	// skirt migration must NOT count as the head — that keeps it in the
	// orchestrator's pending set so Run resumes its post-tx phase
	// instead of skipping it forever (leaving an INVALID index).
	if err := a.ensurePhaseColumn(ctx); err != nil {
		return "", fmt.Errorf("postgres AppliedHead: ensure phase column: %w", err)
	}
	var head string
	err := a.conn.QueryRow(ctx,
		`SELECT COALESCE(to_char(MAX(timestamp) AT TIME ZONE 'UTC', 'YYYYMMDD"T"HH24MISS"Z"'), '') FROM wc_migrations WHERE post_tx_complete`,
	).Scan(&head)
	if err != nil {
		var pgErr *pgconn.PgError
		// 42P01 = undefined_table — fresh DB; the next applied
		// migration's up_sql will CREATE wc_migrations (D27).
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return "", nil
		}
		return "", fmt.Errorf("postgres AppliedHead: %w", err)
	}
	return head, nil
}

// ensurePhaseColumn idempotently adds the Q52 `post_tx_complete`
// column to a pre-existing wc_migrations table (one created before
// this feature shipped). Brand-new DBs get the column from the first
// migration's CREATE TABLE (RenderWcMigrationsCreate), so the ALTER is
// a no-op there; a fresh DB has no table yet (42P01) which is also a
// no-op. Runs at most once per connection — cached via
// phaseColEnsured. The DEFAULT true backfills existing rows as
// "complete", which is correct: every row that predates this feature
// was applied atomically (no skirt-phase concept existed).
func (a *Applier) ensurePhaseColumn(ctx context.Context) error {
	if a.phaseColEnsured {
		return nil
	}
	_, err := a.conn.Exec(ctx,
		`ALTER TABLE wc_migrations ADD COLUMN IF NOT EXISTS post_tx_complete BOOLEAN NOT NULL DEFAULT true`,
		pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		var pgErr *pgconn.PgError
		// 42P01 = undefined_table — fresh DB, nothing to bootstrap; the
		// first migration's CreateTracker emits the column. Not an error.
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			a.phaseColEnsured = true
			return nil
		}
		return err
	}
	a.phaseColEnsured = true
	return nil
}

// MigrationPhase reports the Q52 applied-state phase of migration
// `id` on this target (migrate.ResumableApplier). A missing table or
// missing row reads as PhaseFresh (not an error) so a fresh DB / a
// not-yet-applied migration flows through the normal Apply path.
func (a *Applier) MigrationPhase(ctx context.Context, id string) (migrate.Phase, error) {
	if err := a.ensurePhaseColumn(ctx); err != nil {
		return migrate.PhaseFresh, fmt.Errorf("postgres MigrationPhase: ensure phase column: %w", err)
	}
	var complete bool
	// Canonicalise the stored TIMESTAMPTZ back to the migration id
	// string exactly as AppliedHead does, so equality matches the id
	// the registry stamped (avoids tz/precision round-trip ambiguity).
	err := a.conn.QueryRow(ctx,
		`SELECT post_tx_complete FROM wc_migrations
		   WHERE to_char(timestamp AT TIME ZONE 'UTC', 'YYYYMMDD"T"HH24MISS"Z"') = $1`,
		id).Scan(&complete)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return migrate.PhaseFresh, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return migrate.PhaseFresh, nil // fresh DB — table not created yet
		}
		return migrate.PhaseFresh, fmt.Errorf("postgres MigrationPhase: %w", err)
	}
	if complete {
		return migrate.PhaseComplete, nil
	}
	return migrate.PhasePending, nil
}

// Apply streams the migration's up_sql + up_post_tx into the
// connection.
//
// **up_sql** runs as one Exec — its body has its own explicit
// BEGIN / COMMIT framing so PG executes the whole transactional
// half atomically.
//
// **up_post_tx** is split per-statement and each runs as its own
// Exec. PG's simple-query-protocol wraps multi-statement queries
// in an implicit transaction (per the protocol spec), which would
// re-trap `CREATE INDEX CONCURRENTLY` exactly the way bake-into-
// up_sql did. Per-statement Exec keeps each post-tx op + the
// trailing wc_migrations INSERT in their own auto-commit.
//
// pgx defaults to extended protocol; we pin
// `QueryExecModeSimpleProtocol` so the body is sent verbatim
// (no client-side prepared-statement caching, which would also
// reject CONCURRENTLY).
func (a *Applier) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	if _, err := a.conn.Exec(ctx, m.GetUpSql(), pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("postgres apply up_sql: %w", err)
	}
	return a.applyPostTx(ctx, m)
}

// ApplyPostTx runs ONLY the migration's post-tx half — the Q52 resume
// path (migrate.ResumableApplier). The orchestrator calls this for a
// PhasePending migration whose up_sql already committed (its pending
// wc_migrations row exists); re-running up_sql would wedge on the
// already-created in-tx objects. The post-tx body's leading
// `DROP INDEX CONCURRENTLY IF EXISTS` clears any INVALID index a prior
// crashed CONCURRENTLY left behind, then rebuilds; the trailing UPDATE
// flips the row to complete.
func (a *Applier) ApplyPostTx(ctx context.Context, m *applyfetchpb.Migration) error {
	return a.applyPostTx(ctx, m)
}

// postTxAdvisoryLockKey is the session-level advisory-lock key the
// post-tx skirt serialises on (Q65-engine-1). A fixed key in the
// migrator's own namespace — the skirt is rare + sequential, so a single
// global key (rather than one per migration) is enough and simplest.
const postTxAdvisoryLockKey int64 = 0x77313770737474 // "w17ptt"

// applyPostTx streams up_post_tx statement-by-statement under
// SimpleProtocol. Each runs in its own autocommit so CONCURRENTLY
// stays outside the implicit multi-statement transaction.
//
// Q65-engine-1: the skirt runs OUTSIDE any transaction (CONCURRENTLY
// can't be wrapped), so the wc_migrations PK that serialises the in-tx
// half does NOT protect it — two migrators both resuming the same
// PhasePending migration would each run `DROP INDEX CONCURRENTLY IF
// EXISTS` + `CREATE INDEX CONCURRENTLY` and collide (dropping each
// other's in-progress index / "already exists"). Serialise the skirt on a
// session-level advisory lock (the only lock that survives outside a tx;
// auto-released if the process dies), then RE-CHECK the phase under the
// lock — a concurrent migrator that already completed the skirt (flipping
// post_tx_complete) makes this a clean no-op instead of a collision.
func (a *Applier) applyPostTx(ctx context.Context, m *applyfetchpb.Migration) error {
	stmts := splitStatements(m.GetUpPostTx())
	if len(stmts) == 0 {
		return nil // no skirt — nothing to run or serialise
	}

	if _, err := a.conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, postTxAdvisoryLockKey); err != nil {
		return fmt.Errorf("postgres post-tx advisory lock: %w", err)
	}
	defer func() {
		// Release on the background context so a cancelled ctx still frees
		// the lock (the session would free it on disconnect anyway).
		_, _ = a.conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, postTxAdvisoryLockKey)
	}()

	// Re-check under the lock: a migrator we just waited behind may have
	// already run + completed this skirt. Skip ONLY when the migration is
	// already COMPLETE (a wc_migrations row with post_tx_complete=true) — a
	// concurrent migrator finished it, so re-running would collide.
	//
	// PhaseFresh (no wc_migrations row yet) is a RUN case, not a skip: a
	// PURE post-tx migration (no in-tx DDL — e.g. add-unique / concurrent
	// index synthesised entirely as a CONCURRENTLY skirt) carries its own
	// wc_migrations INSERT in the skirt, so its in-tx half wrote no pending
	// marker. Gating on `== PhasePending` would have skipped it on a fresh
	// apply, silently dropping the index + the bookkeeping row (→ never
	// idempotent). The leading `DROP INDEX CONCURRENTLY IF EXISTS` keeps the
	// skirt safe to re-run after a crash between the index build and the
	// INSERT.
	phase, err := a.MigrationPhase(ctx, m.GetId())
	if err != nil {
		return fmt.Errorf("postgres post-tx phase re-check: %w", err)
	}
	if phase == migrate.PhaseComplete {
		return nil // already fully applied (concurrent migrator / re-run) — idempotent no-op
	}

	for _, stmt := range stmts {
		if _, err := a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("postgres apply up_post_tx: %w", err)
		}
	}
	return nil
}

// Rollback runs the migration's down payload — the inverse of
// Apply. Order: down_pre_tx first (post-tx-equivalent skirt;
// e.g. `DROP INDEX CONCURRENTLY`), then down_sql (in-tx body
// including the wc_migrations DELETE applied.Wrap injected).
//
// down_pre_tx runs statement-by-statement under
// SimpleProtocol — same per-statement / no-prepare contract
// as up_post_tx in Apply, for the same reason (CONCURRENTLY +
// implicit-tx wrapping).
func (a *Applier) Rollback(ctx context.Context, m *applyfetchpb.Migration) error {
	for _, stmt := range splitStatements(m.GetDownPreTx()) {
		if _, err := a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("postgres rollback down_pre_tx: %w", err)
		}
	}
	if down := m.GetDownSql(); down != "" {
		if _, err := a.conn.Exec(ctx, down, pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("postgres rollback down_sql: %w", err)
		}
	}
	return nil
}

// splitStatements splits a post-tx body on `;` STATEMENT boundaries only —
// semicolons inside single-quoted string literals, dollar-quoted strings, line
// comments (`-- …`) and block comments (`/* … */`) are NOT boundaries. Trims
// whitespace + drops empties; preserves the trailing `;` on each statement.
//
// writer-F5: the old naive `strings.Split(sql, ";")` tore a raw CONCURRENTLY
// index whose partial-index predicate / expression carried a literal semicolon
// (e.g. `… WHERE status = 'a;b'`) — a user-authorable escape-hatch shape — into
// two syntactically-broken Execs AFTER the in-tx half already committed the
// pending row, wedging the deploy on every re-run.
func splitStatements(sql string) []string {
	if sql == "" {
		return nil
	}
	var out []string
	var buf strings.Builder
	flush := func() {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if s != "" {
			out = append(out, s+";")
		}
	}
	b := []byte(sql)
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\'': // single-quoted string; '' is an escaped quote
			buf.WriteByte(c)
			i++
			for i < len(b) {
				buf.WriteByte(b[i])
				if b[i] == '\'' {
					if i+1 < len(b) && b[i+1] == '\'' {
						i++
						buf.WriteByte(b[i])
						i++
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '-' && i+1 < len(b) && b[i+1] == '-': // line comment
			for i < len(b) && b[i] != '\n' {
				buf.WriteByte(b[i])
				i++
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*': // block comment (PG nests)
			depth := 0
			for i < len(b) {
				if b[i] == '/' && i+1 < len(b) && b[i+1] == '*' {
					depth++
					buf.WriteString("/*")
					i += 2
					continue
				}
				if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
					depth--
					buf.WriteString("*/")
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				buf.WriteByte(b[i])
				i++
			}
		case c == '$': // dollar-quoted string: $tag$ … $tag$ (tag may be empty)
			if tag := dollarTag(b, i); tag != "" {
				buf.WriteString(tag)
				i += len(tag)
				for i < len(b) {
					if hasPrefixAt(b, i, tag) {
						buf.WriteString(tag)
						i += len(tag)
						break
					}
					buf.WriteByte(b[i])
					i++
				}
			} else {
				buf.WriteByte(c)
				i++
			}
		case c == ';':
			flush()
			i++
		default:
			buf.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

// dollarTag returns the `$tag$` opening delimiter at b[i] (tag ∈ [A-Za-z0-9_]*),
// or "" when b[i] doesn't start one.
func dollarTag(b []byte, i int) string {
	if b[i] != '$' {
		return ""
	}
	j := i + 1
	for j < len(b) && (b[j] == '_' || (b[j] >= 'a' && b[j] <= 'z') || (b[j] >= 'A' && b[j] <= 'Z') || (b[j] >= '0' && b[j] <= '9')) {
		j++
	}
	if j < len(b) && b[j] == '$' {
		return string(b[i : j+1])
	}
	return ""
}

func hasPrefixAt(b []byte, i int, s string) bool {
	return i+len(s) <= len(b) && string(b[i:i+len(s)]) == s
}

// Wipe drops every object in the public schema and recreates it empty
// (migrate.Wiper) — the dev fresh-build primitive. `DROP SCHEMA public
// CASCADE` removes all tables (incl. wc_migrations), types, indexes,
// etc.; `CREATE SCHEMA public` restores the empty default schema. Only
// the connected database is touched.
func (a *Applier) Wipe(ctx context.Context) error {
	if _, err := a.conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public",
		pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("postgres Wipe: %w", err)
	}
	a.phaseColEnsured = false
	return nil
}

// Close releases the underlying pgx connection. Idempotent —
// double-Close on pgx.Conn is safe (closes once, returns nil
// thereafter).
func (a *Applier) Close() error {
	if a.conn == nil {
		return nil
	}
	err := a.conn.Close(context.Background())
	a.conn = nil
	return err
}

// Fingerprint extracts the canonical PG schema state via
// information_schema and returns its hex-encoded sha256
// (Phase D — D-iter3-14). Excludes the wc_migrations
// bookkeeping table; sorted by name + columns. The
// orchestrator's Phase D drift check calls this before each
// pending migration; for now the comparison is stubbed to
// always-pass (real fingerprints land when console grows
// shadow-DB integration).
func (a *Applier) Fingerprint(ctx context.Context) (string, error) {
	schema, err := fingerprint.ExtractPostgres(ctx, a.conn)
	if err != nil {
		return "", err
	}
	return schema.FingerprintHex(), nil
}
