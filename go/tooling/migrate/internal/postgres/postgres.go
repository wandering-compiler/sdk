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
	"github.com/wandering-compiler/sdk/go/lib/dbsession"
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

	// phaseColEnsured caches the idempotent phase-column bootstrap (ALTER TABLE
	// … ADD COLUMN IF NOT EXISTS post_tx_complete) so it runs at most
	// once per connection. See ensurePhaseColumn.
	phaseColEnsured bool
}

// Compile-time check the impl satisfies the migrate.Applier
// contract + the optional FingerprintCapable for drift
// detection + the optional ResumableApplier for two-phase
// crash recovery.
var _ migrate.Applier = (*Applier)(nil)
var _ migrate.Wiper = (*Applier)(nil)
var _ migrate.FingerprintCapable = (*Applier)(nil)
var _ migrate.SeedCapable = (*Applier)(nil)
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
	// Pin the session settings the emitted SQL depends on,
	// from the same declaration the generated runtime reads. The applier
	// was NOT self-guarding here, contrary to how it looked: pgx checks
	// standard_conforming_strings inside sanitizeForSimpleQuery, which
	// execSimpleProtocol calls only when a query HAS bind arguments —
	// migration bodies have none, so their literals were parsed under
	// whatever the session happened to carry. search_path matters for the
	// same reason in the other direction: bare identifiers (including
	// `w17_migrations`) resolve through it, so an applier running as a role
	// that owns a same-named schema creates the tables somewhere the
	// runtime never looks.
	conn, err := pgx.Connect(ctx, dbsession.ApplyPGDSNParams(dsn))
	if err != nil {
		return nil, fmt.Errorf("postgres.New: %w", err)
	}
	return &Applier{conn: conn, dsn: dsn}, nil
}

// AppliedHead returns the id of the most recently applied
// migration on this DB by querying `w17_migrations`.
// Missing table = empty string (treated as fresh DB by the
// orchestrator).
//
// `w17_migrations.timestamp` is TIMESTAMPTZ; the orchestrator's
// pending filter compares against `naming.Name` strings
// (`YYYYMMDDTHHMMSSZ` basic ISO-8601). We format here via
// `to_char` so the returned string round-trips against the id
// the registry stamped on each Migration.
// IsPostgres satisfies migrate.PostgresDialect, so the extension preflight
// knows its `DO $$ … pg_extension` probe means something here. The manifest
// field it reads travels on every dialect for tracking, so the probe has to
// ask rather than assume.
func (a *Applier) IsPostgres() bool { return true }

func (a *Applier) AppliedHead(ctx context.Context) (string, error) {
	// Exclude incomplete (post_tx_complete=false) rows from the
	// head. A partially-applied skirt migration must NOT count as the
	// head — that keeps it in the orchestrator's pending set so Run
	// resumes its post-tx phase instead of skipping it forever (leaving
	// an INVALID index).
	//
	// READ-ONLY. `status` and every plan come through here, so this must
	// not change the ledger: a ledger that predates the column is read
	// as it is (every row it holds is complete — see ensurePhaseColumn),
	// and the column is added only on the apply path. An ALTER here made
	// a status probe need ALTER privilege, mutate the ledger, and take an
	// ACCESS EXCLUSIVE lock that queues behind an in-flight migration.
	hasPhase, err := a.hasPhaseColumn(ctx)
	if err != nil {
		return "", fmt.Errorf("postgres AppliedHead: %w", err)
	}
	query := `SELECT COALESCE(to_char(MAX(timestamp) AT TIME ZONE 'UTC', 'YYYYMMDD"T"HH24MISS"Z"'), '') FROM w17_migrations`
	if hasPhase {
		query += ` WHERE post_tx_complete`
	}
	var head string
	err = a.conn.QueryRow(ctx, query).Scan(&head)
	if err != nil {
		var pgErr *pgconn.PgError
		// 42P01 = undefined_table — fresh DB; the next applied
		// migration's up_sql will CREATE w17_migrations.
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return "", nil
		}
		return "", fmt.Errorf("postgres AppliedHead: %w", err)
	}
	return head, nil
}

// hasPhaseColumn reports whether the ledger carries the
// `post_tx_complete` column, from the catalogue, without touching the
// ledger. A missing ledger answers false (the caller's query then reports
// the fresh database as it always has). The table is resolved through the
// session's search_path, exactly as the unqualified queries resolve it.
func (a *Applier) hasPhaseColumn(ctx context.Context) (bool, error) {
	if a.phaseColEnsured {
		return true, nil
	}
	var has bool
	err := a.conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_attribute
		   WHERE attrelid = to_regclass('w17_migrations')
		     AND attname = 'post_tx_complete' AND NOT attisdropped)`,
	).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("read the ledger's columns: %w", err)
	}
	return has, nil
}

// ensurePhaseColumn idempotently adds the `post_tx_complete`
// column to a pre-existing w17_migrations table (one created before
// this feature shipped). Brand-new DBs get the column from the first
// migration's CREATE TABLE (RenderW17MigrationsCreate), so the ALTER is
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
		`ALTER TABLE w17_migrations ADD COLUMN IF NOT EXISTS post_tx_complete BOOLEAN NOT NULL DEFAULT true`,
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

// MigrationPhase reports the applied-state phase of migration
// `id` on this target (migrate.ResumableApplier). A missing table or
// missing row reads as PhaseFresh (not an error) so a fresh DB / a
// not-yet-applied migration flows through the normal Apply path.
func (a *Applier) MigrationPhase(ctx context.Context, id string) (migrate.Phase, error) {
	// Read-only for the same reason as AppliedHead: a ledger without the
	// column holds only complete rows.
	hasPhase, err := a.hasPhaseColumn(ctx)
	if err != nil {
		return migrate.PhaseFresh, fmt.Errorf("postgres MigrationPhase: %w", err)
	}
	completeExpr := "post_tx_complete"
	if !hasPhase {
		completeExpr = "true"
	}
	var complete bool
	// Canonicalise the stored TIMESTAMPTZ back to the migration id
	// string exactly as AppliedHead does, so equality matches the id
	// the registry stamped (avoids tz/precision round-trip ambiguity).
	err = a.conn.QueryRow(ctx,
		`SELECT `+completeExpr+` FROM w17_migrations
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
// trailing w17_migrations INSERT in their own auto-commit.
//
// pgx defaults to extended protocol; we pin
// `QueryExecModeSimpleProtocol` so the body is sent verbatim
// (no client-side prepared-statement caching, which would also
// reject CONCURRENTLY).
func (a *Applier) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	// The one place a legacy ledger is upgraded: this body is about to
	// write a ledger row that may name the column.
	if err := a.ensurePhaseColumn(ctx); err != nil {
		return fmt.Errorf("postgres apply: ensure phase column: %w", err)
	}
	if _, err := a.conn.Exec(ctx, m.GetUpSql(), pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("postgres apply up_sql: %w", err)
	}
	return a.applyPostTx(ctx, m)
}

// ApplyPostTx runs ONLY the migration's post-tx half — the resume
// path (migrate.ResumableApplier). The orchestrator calls this for a
// PhasePending migration whose up_sql already committed (its pending
// w17_migrations row exists); re-running up_sql would wedge on the
// already-created in-tx objects. The post-tx body's leading
// `DROP INDEX CONCURRENTLY IF EXISTS` clears any INVALID index a prior
// crashed CONCURRENTLY left behind, then rebuilds; the trailing UPDATE
// flips the row to complete.
func (a *Applier) ApplyPostTx(ctx context.Context, m *applyfetchpb.Migration) error {
	return a.applyPostTx(ctx, m)
}

// postTxAdvisoryLockKey is the session-level advisory-lock key the
// post-tx skirt serialises on. A fixed key in the
// migrator's own namespace — the skirt is rare + sequential, so a single
// global key (rather than one per migration) is enough and simplest.
const postTxAdvisoryLockKey int64 = 0x77313770737474 // "w17ptt"

// applyPostTx streams up_post_tx statement-by-statement under
// SimpleProtocol. Each runs in its own autocommit so CONCURRENTLY
// stays outside the implicit multi-statement transaction.
//
// The skirt runs OUTSIDE any transaction (CONCURRENTLY
// can't be wrapped), so the w17_migrations PK that serialises the in-tx
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
	// already COMPLETE (a w17_migrations row with post_tx_complete=true) — a
	// concurrent migrator finished it, so re-running would collide.
	//
	// PhaseFresh (no w17_migrations row yet) is a RUN case, not a skip: a
	// PURE post-tx migration (no in-tx DDL — e.g. add-unique / concurrent
	// index synthesised entirely as a CONCURRENTLY skirt) carries its own
	// w17_migrations INSERT in the skirt, so its in-tx half wrote no pending
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
// including the w17_migrations DELETE applied.Wrap injected).
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
// The old naive `strings.Split(sql, ";")` tore a raw CONCURRENTLY
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
		case c == '\'' || c == '"':
			// Single-quoted string or quoted identifier; a doubled quote is
			// an escaped one. An E'…' string also escapes with a backslash,
			// so `E'it\'s; fine'` is one literal, not a boundary.
			backslash := c == '\'' && i > 0 && (b[i-1] == 'E' || b[i-1] == 'e') &&
				(i < 2 || !identByte(b[i-2]))
			buf.WriteByte(c)
			i++
			for i < len(b) {
				buf.WriteByte(b[i])
				if backslash && b[i] == '\\' && i+1 < len(b) {
					i++
					buf.WriteByte(b[i])
					i++
					continue
				}
				if b[i] == c {
					if i+1 < len(b) && b[i+1] == c {
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
			if tag := dollarTag(b, i); tag != "" && (i == 0 || !identByte(b[i-1])) {
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

// dollarTag returns the `$tag$` opening delimiter at b[i] (tag ∈ identifier
// bytes — [A-Za-z0-9_] and UTF-8 runes — not starting with a digit), or "" when b[i] doesn't start one.
func dollarTag(b []byte, i int) string {
	if b[i] != '$' {
		return ""
	}
	j := i + 1
	if j < len(b) && b[j] >= '0' && b[j] <= '9' {
		return "" // a positional parameter ($1), not a quote
	}
	// A tag follows identifier rules, and Postgres lets a multi-byte UTF-8
	// rune into an identifier: `$é$` is a tag too. Read as no tag, its body
	// was split at every `;` inside it.
	for j < len(b) && (b[j] == '_' || b[j] >= 0x80 || (b[j] >= 'a' && b[j] <= 'z') || (b[j] >= 'A' && b[j] <= 'Z') || (b[j] >= '0' && b[j] <= '9')) {
		j++
	}
	if j < len(b) && b[j] == '$' {
		return string(b[i : j+1])
	}
	return ""
}

// identByte reports whether c can continue an unquoted identifier, so a `$`
// or an `E` before a quote that belongs to a longer word is not read as
// syntax.
func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func hasPrefixAt(b []byte, i int, s string) bool {
	return i+len(s) <= len(b) && string(b[i:i+len(s)]) == s
}

// Wipe returns the connected database to the empty state (migrate.Wiper) —
// the dev fresh-build primitive. It drops `public` and each DECLARED
// namespace (the ones schema prep creates for the migration set, see
// migrate.DeclaredNamespaces), then recreates an empty `public`; everything
// inside goes with them (tables incl. w17_migrations, types, functions,
// extensions installed there). One simple-protocol batch, so it runs as one
// implicit transaction: a failure leaves the database as it was.
//
// `public` alone was not enough. Schema prep creates each namespace a
// module's tables are qualified with (`billing`, `ledger`), and a wipe that
// left them behind left their tables too — the next fresh build then found
// a populated store and `CREATE TABLE "billing"."invoices"` failed on a
// database that was supposed to be empty.
//
// Declared namespaces only, never "whatever this role may drop": a dev
// stack usually connects as a superuser, which may drop every schema in the
// database, including ones other tools keep there. A schema the migration
// set does not declare stays. A system schema is refused. Only the connected
// database is touched, and no session setting is changed.
func (a *Applier) Wipe(ctx context.Context, namespaces []string) error {
	stmt, err := wipeSQL(namespaces)
	if err != nil {
		return fmt.Errorf("postgres Wipe: %w", err)
	}
	if _, err := a.conn.Exec(ctx, stmt, pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("postgres Wipe: %w", err)
	}
	a.phaseColEnsured = false
	return nil
}

// wipeSQL drops public and the given schemas and recreates an empty public.
// `public` and duplicates are folded; a system schema is refused.
func wipeSQL(schemas []string) (string, error) {
	var b strings.Builder
	b.WriteString("DROP SCHEMA IF EXISTS public CASCADE;\n")
	seen := map[string]bool{"public": true}
	for _, s := range schemas {
		if s = strings.TrimSpace(s); s == "" || seen[s] {
			continue
		}
		if s == "information_schema" || strings.HasPrefix(s, "pg_") {
			return "", fmt.Errorf("refusing to drop system schema %q", s)
		}
		seen[s] = true
		b.WriteString("DROP SCHEMA IF EXISTS " + pgx.Identifier{s}.Sanitize() + " CASCADE;\n")
	}
	b.WriteString("CREATE SCHEMA public;")
	return b.String(), nil
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
// information_schema and returns its hex-encoded sha256.
// Excludes the w17_migrations
// bookkeeping table; sorted by name + columns. The
// orchestrator's drift check calls this before each
// pending migration and REFUSES on a mismatch. The comparison is
// skipped only when the migration carries no
// `expected_pre_fingerprint` or a `FAKE_` placeholder, which is
// what console still emits until it grows shadow-DB integration
// — so the gap is in what console PROMISES, not in whether this
// is checked.
func (a *Applier) Fingerprint(ctx context.Context) (string, error) {
	schema, err := fingerprint.ExtractPostgres(ctx, a.conn)
	if err != nil {
		return "", err
	}
	return schema.FingerprintHex(), nil
}

// ExecSeed runs a rendered fixture's upserts in one transaction, satisfying
// migrate.SeedCapable.
//
// One transaction because a fixture is a SET: the renderer emits rows in FK
// order (targets before referrers), and a run that stops halfway leaves a
// database in a state no fixture describes — with referrers missing and
// nothing saying which ones. Rolling back is the only honest partial outcome.
//
// The statements arrive parameterized ($1..$N with args alongside) and are
// executed that way. Nothing here builds SQL, so a fixture value carrying a
// quote is a value rather than syntax.
func (a *Applier) ExecSeed(ctx context.Context, stmts []migrate.SeedStmt) error {
	if len(stmts) == 0 {
		return nil
	}
	tx, err := a.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres seed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i, st := range stmts {
		if _, err := tx.Exec(ctx, st.SQL, st.Args...); err != nil {
			return fmt.Errorf("postgres seed: statement %d of %d: %w", i+1, len(stmts), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres seed: commit: %w", err)
	}
	return nil
}

// Observe reports this database's live schema as structure — see
// migrate.ObserveCapable. Same extraction the fingerprint uses, in the shape
// that keeps namespaces, so a table a module put in its own schema is reported
// rather than silently missing.
func (a *Applier) Observe(ctx context.Context) (fingerprint.Observed, error) {
	return fingerprint.ObservePostgres(ctx, a.conn)
}

var _ migrate.CheckRunner = (*Applier)(nil)

// RunCheck runs one pre-apply check (migrate.CheckRunner).
//
// Only a single read-only SELECT is accepted (migrate.ValidateMigrationCheckSQL), and
// it runs on a connection of its OWN — never the applier's, which holds the
// run lock (a check calling pg_advisory_unlock_all() there would drop it) —
// inside a READ ONLY transaction with a statement and a lock timeout, always
// rolled back. A failure reports the SQLSTATE only: the server's message can
// carry the data the probe read (a failed cast quotes the value).
func (a *Applier) RunCheck(ctx context.Context, query, body string) (int64, error) {
	if err := migrate.ValidateMigrationCheckSQL(query, body, migrate.CheckPostgres); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, migrate.CheckTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbsession.ApplyPGDSNParams(a.dsn))
	if err != nil {
		return 0, fmt.Errorf("check connection: %w", pgCheckErr(err))
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, pgCheckErr(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, st := range []string{
		fmt.Sprintf("SET LOCAL statement_timeout = %d", migrate.CheckTimeout.Milliseconds()),
		"SET LOCAL lock_timeout = 5000",
	} {
		if _, err := tx.Exec(ctx, st); err != nil {
			return 0, pgCheckErr(err)
		}
	}
	// temp_file_limit bounds the disk a probe's sort or hash may spill. It is
	// a superuser setting, so a migration role may not be allowed to lower it:
	// tried under a savepoint, and the check runs on without it (bounded by
	// the statement timeout) when the server refuses.
	if _, err := tx.Exec(ctx, "SAVEPOINT w17_check_limits"); err != nil {
		return 0, pgCheckErr(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL temp_file_limit = '1GB'"); err != nil {
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT w17_check_limits"); err != nil {
			return 0, pgCheckErr(err)
		}
	}
	var n int64
	if err := tx.QueryRow(ctx, migrate.CheckCountSQL(query)).Scan(&n); err != nil {
		return 0, pgCheckErr(err)
	}
	return n, nil
}

// pgCheckErr reduces a check failure to its SQLSTATE.
func pgCheckErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("the check failed (SQLSTATE %s)", pe.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("the check timed out")
	}
	return errors.New("the check failed")
}
