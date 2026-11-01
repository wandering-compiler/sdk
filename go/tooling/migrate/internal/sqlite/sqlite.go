// Package sqlite is the production-side SQLite Applier for
// w17ctl. Uses `modernc.org/sqlite` — pure Go, CGO-free,
// so w17ctl stays statically buildable on any host.
//
// DSN forms accepted at the w17ctl surface:
//
//   - `sqlite:///abs/path.db?param=value` — URL form, leading
//     `sqlite://` stripped.
//   - `sqlite://relative/path.db` — URL form, treated as
//     `relative/path.db`.
//   - `file:rel-or-abs.db?param=value` — modernc.org/sqlite
//     native form, passed through.
//
// Foreign-key handling: SQLite's 12-step rebuild recipe (the
// migrator's structured ALTER TABLE path) requires
// `PRAGMA foreign_keys=OFF` BEFORE the BEGIN that wraps the
// rebuild, then `=ON` after COMMIT. We toggle the pragma per-
// migration so the migrator's emit body works as-is.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/dbsession"
	"github.com/wandering-compiler/sdk/go/lib/sqlitecollate"
	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Applier is the per-connection SQLite apply driver.
type Applier struct {
	db *sql.DB
}

var _ migrate.Applier = (*Applier)(nil)
var _ migrate.Wiper = (*Applier)(nil)
var _ migrate.FingerprintCapable = (*Applier)(nil)

// New opens a sql.DB against the resolved file path. Eagerly
// pings so a missing-directory typo surfaces at construction.
func New(ctx context.Context, dsn string) (*Applier, error) {
	if dsn == "" {
		return nil, fmt.Errorf("sqlite.New: dsn is empty")
	}
	// Register the W17_UNICODE collation before opening, so
	// `CREATE TABLE ... COLLATE W17_UNICODE` DDL resolves (SQLite binds a
	// column's collation at CREATE time) and the indexes the migrator builds
	// order text identically to the runtime binary's queries. Global +
	// idempotent, so applying several SQLite connections is fine.
	sqlitecollate.Register()
	driverDSN := URLToDriverDSN(dsn)
	db, err := sql.Open("sqlite", driverDSN)
	if err != nil {
		return nil, fmt.Errorf("sqlite.New: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite.New: ping: %w", err)
	}
	return &Applier{db: db}, nil
}

// AppliedHead returns the id of the most recently applied
// migration on this DB by querying `w17_migrations`.
// Missing table = empty string (treated as fresh DB by the
// orchestrator). SQLite returns "no such table: w17_migrations"
// on absence, sniffed via substring match on the error message
// (modernc.org/sqlite does not expose typed error codes).
func (a *Applier) AppliedHead(ctx context.Context) (string, error) {
	var head sql.NullString
	err := a.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(timestamp), '') FROM w17_migrations`,
	).Scan(&head)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return "", nil
		}
		return "", fmt.Errorf("sqlite AppliedHead: %w", err)
	}
	return head.String, nil
}

// Apply executes the migration body. Wraps the apply with a
// `PRAGMA foreign_keys=OFF` … `=ON` pair so the structured
// ALTER TABLE rebuild recipe (drop + recreate + copy + rename)
// can run inside the migration's own BEGIN / COMMIT block
// without dependent FKs cascading. The pragma is connection-
// scoped; we use a single Conn so the OFF setting reaches the
// BEGIN that follows.
//
// RESUME-GAP INVARIANT: unlike Postgres, SQLite has no
// PhasePending recovery here — AppliedHead does not filter on a
// `post_tx_complete` marker, and up_post_tx runs as a SEPARATE Exec
// after up_sql has already committed. In the skirt layout the
// w17_migrations bookkeeping row is written at the END of up_post_tx
// (applied.Wrap's legacy-skirt path), NOT in up_sql. So a crash BETWEEN
// the two Execs leaves up_sql's DDL committed with NO applied-state row:
// a re-run sees the migration as un-applied and re-executes up_sql, which
// then WEDGES LOUDLY on the already-committed DDL. This is tolerable only
// because up_post_tx is normally empty for SQLite: the migrator routes
// ops into the post-tx bucket solely when an index op carries
// non_transactional=true (= a CONCURRENTLY index), a Postgres-only
// construct — SQLite builds indexes online with no such syntax. When a
// non-empty up_post_tx does reach this dialect we emit a WARNING.
func (a *Applier) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	if err := a.stampCollation(ctx); err != nil {
		return err
	}
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite apply: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// B4: enforce foreign keys for THIS migration. modernc.org/sqlite (like
	// SQLite generally) defaults foreign_keys=OFF per connection, so without
	// this an INSERT/UPDATE that violates an FK slips in silently. A rebuild
	// migration's own up_sql brackets its 12-step recipe with
	// `PRAGMA foreign_keys=OFF … =ON` (emit's TxBracket) — that OFF window
	// runs *inside* the up_sql script below and re-enables at its tail, so
	// setting ON here is the correct baseline: plain migrations enforce FK,
	// rebuilds toggle it themselves. The applier no longer wraps EVERY
	// migration in a blanket OFF (which disabled enforcement for all of them).
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON;"); err != nil {
		return fmt.Errorf("sqlite apply: PRAGMA foreign_keys=ON: %w", err)
	}
	before, err := observeForeignKeysBefore(ctx, conn, m.GetUpSql())
	if err != nil {
		return fmt.Errorf("sqlite apply (migration %s): %w", m.GetId(), err)
	}
	if _, err := conn.ExecContext(ctx, m.GetUpSql()); err != nil {
		return fmt.Errorf("sqlite apply up_sql: %w", err)
	}
	if pt := m.GetUpPostTx(); pt != "" {
		slog.Warn("sqlite: running non-empty up_post_tx as a separate step after up_sql committed; a crash between them is NOT resumable — up_sql's DDL commits with no applied-state row (recorded at the end of up_post_tx), so a re-run re-executes up_sql and wedges on the already-committed DDL",
			slog.String("migration_id", m.GetId()))
		if _, err := conn.ExecContext(ctx, pt); err != nil {
			return fmt.Errorf("sqlite apply up_post_tx: %w", err)
		}
	}
	// B4: a 12-step rebuild runs its body under `foreign_keys=OFF`, so any FK
	// violation it introduces (e.g. adding a stricter FK over existing data) is
	// NOT rejected during apply. The emit body embeds `PRAGMA foreign_key_check`
	// before its COMMIT, but that runs via ExecContext, which DISCARDS the
	// result rows — the check was decorative. Re-run it here as a QUERY and fail
	// on any violation so a broken rebuild surfaces loudly instead of leaving a
	// silently-inconsistent database. (Detection is post-commit: the rebuild is
	// already committed, so the operator must repair the data — but it is
	// detected + loud, not silent.)
	//
	// Only violations the migration INTRODUCED fail it: more orphans of a key
	// the database already had. A key the migration ADDS over a table that
	// already held orphans is the case the migrator tolerates
	// (fk-add-over-existing-rows) — Postgres adds it NOT VALID, MySQL with the
	// checks off — so its orphans are reported, not refused; and orphans an
	// earlier such key left behind must not fail every later rebuild.
	if err := assertNoForeignKeyViolations(ctx, conn, m.GetUpSql(), m.GetId(), before); err != nil {
		return err
	}
	return nil
}

// fkKey identifies a foreign key by what it constrains, not by its position:
// the child table, its columns, the parent table and the parent's columns
// (empty when the key names the parent's primary key implicitly). A rebuild
// renumbers pragma_foreign_key_list's ids, and a table's rowids with it, so
// neither can tell one key, or one orphan, from another across the apply.
type fkKey struct{ table, from, parent, to string }

func (k fkKey) String() string {
	return fmt.Sprintf("%s(%s) -> %s(%s)", k.table, k.from, k.parent, k.to)
}

// fkState is a database's foreign keys and how many rows violate each.
type fkState struct {
	keys       map[fkKey]bool
	violations map[fkKey]int
}

// observeForeignKeysBefore reads the database's foreign keys and their
// violations ahead of a rebuild migration (one carrying the rebuild's
// `PRAGMA foreign_key_check` marker); nil for any other, which enforces its
// keys inline and is not checked after.
func observeForeignKeysBefore(ctx context.Context, conn *sql.Conn, body string) (*fkState, error) {
	if !strings.Contains(body, "PRAGMA foreign_key_check") {
		return nil, nil
	}
	return observeForeignKeys(ctx, conn)
}

// observeForeignKeys reads every user table's foreign keys
// (pragma_foreign_key_list) and counts the rows `PRAGMA foreign_key_check`
// reports against each.
func observeForeignKeys(ctx context.Context, conn *sql.Conn) (*fkState, error) {
	st := &fkState{keys: map[fkKey]bool{}, violations: map[fkKey]int{}}
	tables, err := queryStrings(ctx, conn, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	byID := map[string]map[int64]fkKey{} // lower(table) → key id → key
	for _, t := range tables {
		ids, err := tableForeignKeys(ctx, conn, t)
		if err != nil {
			return nil, err
		}
		byID[strings.ToLower(t)] = ids
		for _, k := range ids {
			st.keys[k] = true
		}
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check;")
	if err != nil {
		return nil, fmt.Errorf("PRAGMA foreign_key_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var id int64
		if err := rows.Scan(&table, &rowid, &parent, &id); err != nil {
			return nil, fmt.Errorf("PRAGMA foreign_key_check: %w", err)
		}
		k, ok := byID[strings.ToLower(table)][id]
		if !ok {
			// A key the listing did not show (cannot happen on one
			// connection between two reads): counted under a key no earlier
			// state holds, so it is never mistaken for an old one's.
			k = fkKey{table: strings.ToLower(table), parent: strings.ToLower(parent), from: fmt.Sprintf("#%d", id)}
		}
		st.violations[k]++
	}
	return st, rows.Err()
}

// tableForeignKeys maps each of table's foreign key ids to its fkKey.
func tableForeignKeys(ctx context.Context, conn *sql.Conn, table string) (map[int64]fkKey, error) {
	rows, err := conn.QueryContext(ctx, `SELECT id, "table", "from", "to" FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table)
	if err != nil {
		return nil, fmt.Errorf("foreign keys of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	type acc struct {
		parent   string
		from, to []string
	}
	keys := map[int64]*acc{}
	var order []int64
	for rows.Next() {
		var id int64
		var parent, from string
		var to sql.NullString
		if err := rows.Scan(&id, &parent, &from, &to); err != nil {
			return nil, fmt.Errorf("foreign keys of %s: %w", table, err)
		}
		a, ok := keys[id]
		if !ok {
			a = &acc{parent: parent}
			keys[id] = a
			order = append(order, id)
		}
		a.from = append(a.from, strings.ToLower(from))
		a.to = append(a.to, strings.ToLower(to.String))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("foreign keys of %s: %w", table, err)
	}
	out := make(map[int64]fkKey, len(keys))
	for _, id := range order {
		a := keys[id]
		to := strings.Join(a.to, ",")
		if strings.Trim(to, ",") == "" {
			to = ""
		}
		out[id] = fkKey{table: strings.ToLower(table), from: strings.Join(a.from, ","), parent: strings.ToLower(a.parent), to: to}
	}
	return out, nil
}

func queryStrings(ctx context.Context, conn *sql.Conn, q string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// assertNoForeignKeyViolations runs `PRAGMA foreign_key_check` after a
// rebuild migration — detected by the `PRAGMA foreign_key_check` marker the
// emit body embeds for the 12-step recipe (a non-rebuild migration enforces FKs
// inline under `foreign_keys=ON`, so there is nothing extra to verify and no
// reason to pay for the scan) — and fails it on a violation it introduced:
// a key the database had before (before, read by observeForeignKeysBefore)
// with more violating rows than it had then. A key the migration added is
// reported with its count and left as it is: the table held those rows
// already, and the migrator adds such a key without refusing them
// (fk-add-over-existing-rows). New writes are checked under foreign_keys=ON
// either way.
func assertNoForeignKeyViolations(ctx context.Context, conn *sql.Conn, body, migID string, before *fkState) error {
	if before == nil || !strings.Contains(body, "PRAGMA foreign_key_check") {
		return nil
	}
	after, err := observeForeignKeys(ctx, conn)
	if err != nil {
		return fmt.Errorf("sqlite apply (migration %s): %w", migID, err)
	}
	var introduced []string
	for k, n := range after.violations {
		if !before.keys[k] {
			slog.Warn("sqlite: a foreign key this migration added over existing rows has orphans: the rows were left as they are (no parent row matches them) and new writes are checked; repair them without deleting — re-create the missing parent or end the child",
				slog.String("migration_id", migID), slog.String("foreign_key", k.String()), slog.Int("orphan_rows", n))
			continue
		}
		if n > before.violations[k] {
			introduced = append(introduced, fmt.Sprintf("%s: %d row(s), %d before", k, n, before.violations[k]))
		}
	}
	if len(introduced) > 0 {
		return fmt.Errorf("sqlite apply: migration %s introduced foreign-key violation(s) — the rebuild ran under foreign_keys=OFF and PRAGMA foreign_key_check found dangling reference(s) the database did not hold before (%s); the schema change is committed but the data is now inconsistent (repair the offending rows, or restore and re-run against clean data)",
			migID, strings.Join(introduced, "; "))
	}
	return nil
}

// Rollback runs the migration's down payload. Order:
// down_pre_tx first, then down_sql (in-tx body including the
// w17_migrations DELETE applied.Wrap injected).
// PRAGMA foreign_keys=OFF/ON wraps the body the same way Apply
// does — the rebuild recipe uses identical drop/recreate/copy
// dance for down direction (the migrator's structured ALTER
// TABLE down).
func (a *Applier) Rollback(ctx context.Context, m *applyfetchpb.Migration) error {
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite rollback: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// B4: same posture as Apply — foreign keys ON by default; a rebuild's own
	// down body brackets its recipe with `foreign_keys=OFF … =ON`. No blanket
	// disable across the whole rollback.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON;"); err != nil {
		return fmt.Errorf("sqlite rollback: PRAGMA foreign_keys=ON: %w", err)
	}
	before, err := observeForeignKeysBefore(ctx, conn, m.GetDownSql()+m.GetDownPreTx())
	if err != nil {
		return fmt.Errorf("sqlite rollback (migration %s): %w", m.GetId(), err)
	}
	if pre := m.GetDownPreTx(); pre != "" {
		if _, err := conn.ExecContext(ctx, pre); err != nil {
			return fmt.Errorf("sqlite rollback down_pre_tx: %w", err)
		}
	}
	if down := m.GetDownSql(); down != "" {
		if _, err := conn.ExecContext(ctx, down); err != nil {
			return fmt.Errorf("sqlite rollback down_sql: %w", err)
		}
	}
	// B4: a rebuild rollback runs under foreign_keys=OFF too — verify it left no
	// dangling references (checks down_sql, then down_pre_tx, for the marker).
	if err := assertNoForeignKeyViolations(ctx, conn, m.GetDownSql()+m.GetDownPreTx(), m.GetId(), before); err != nil {
		return err
	}
	return nil
}

// Wipe drops every user table (migrate.Wiper) — the dev fresh-build
// primitive. Enumerates sqlite_master (skipping SQLite's internal
// sqlite_* objects) and DROPs each table with foreign_keys off so
// dependency order doesn't matter.
func (a *Applier) Wipe(ctx context.Context, _ []string) error {
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite Wipe: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF;"); err != nil {
		return fmt.Errorf("sqlite Wipe: %w", err)
	}
	// List tables in a closure so `defer rows.Close()` releases the read
	// cursor BEFORE the DROP loop (sqlite holds a read lock otherwise) and
	// rows.Err() is checked — the rows must close before the writes.
	listTables := func() ([]string, error) {
		rows, err := conn.QueryContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			names = append(names, n)
		}
		return names, rows.Err()
	}
	names, err := listTables()
	if err != nil {
		return fmt.Errorf("sqlite Wipe: list tables: %w", err)
	}
	for _, n := range names {
		if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS "`+n+`";`); err != nil {
			return fmt.Errorf("sqlite Wipe: drop %s: %w", n, err)
		}
	}
	_, _ = conn.ExecContext(ctx, "PRAGMA foreign_keys=ON;")
	return nil
}

// Close releases the underlying sql.DB. Idempotent.
func (a *Applier) Close() error {
	if a.db == nil {
		return nil
	}
	err := a.db.Close()
	a.db = nil
	return err
}

// Fingerprint extracts the canonical SQLite schema state via
// sqlite_master + pragma_table_info and returns its hex-encoded
// sha256. Excludes the w17_migrations
// bookkeeping table + SQLite's internal sqlite_* tables; sorted
// by name + columns.
func (a *Applier) Fingerprint(ctx context.Context) (string, error) {
	schema, err := fingerprint.ExtractSQLite(ctx, a.db)
	if err != nil {
		return "", err
	}
	return schema.FingerprintHex(), nil
}

// URLToDriverDSN converts the w17ctl-accepted DSN forms into the form
// modernc.org/sqlite expects, and pins the session settings the emitted
// SQL depends on. Exposed for tests.
//
//   - `sqlite:///abs/path.db` → `/abs/path.db`
//   - `sqlite://relative.db`  → `relative.db`
//   - `file:relative.db?…`    → unchanged (driver-native)
//   - anything else           → unchanged (pass through; driver
//     surfaces a clear error if the form is wrong)
//
// The pragmas come from [dbsession.SQLitePragmas], the same declaration
// the generated runtime renders, because the applier and the runtime
// disagreeing about the session is a defect either way round: one such
// defect was LIKE meaning one thing here and another at serving time, with CHECK
// expressions and partial-index predicates evaluated on this connection.
// Asking the shared declaration is what keeps the next pin from having
// to remember this call site exists.
func URLToDriverDSN(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "sqlite:///"):
		dsn = dsn[len("sqlite://"):] // keep leading "/"
	case strings.HasPrefix(dsn, "sqlite://"):
		dsn = dsn[len("sqlite://"):]
	}
	return dbsession.ApplySQLitePragmas(dsn)
}

// stampCollation records the collator this binary sorts with, and refuses
// to apply on top of a database whose indexes were built with a different
// one.
//
// The stamp lives in the DB, next to the migrations. The comparison itself
// lives in sqlitecollate, next to the collation it characterises, so the
// applier and every generated runtime read ONE implementation of "do these
// two builds order text the same way" — writing it twice is how the two
// copies drift apart.
//
// Refusing HERE, at apply, because this is the moment the damage would be
// done: applying DDL under a different collator is what builds the
// mismatched index.
func (a *Applier) stampCollation(ctx context.Context) error {
	if err := sqlitecollate.EnsureStamp(ctx, a.db); err != nil {
		return fmt.Errorf("sqlite apply: %w", err)
	}
	return nil
}

var _ migrate.CheckRunner = (*Applier)(nil)

// RunCheck runs one pre-apply check (migrate.CheckRunner).
//
// Only a single read-only SELECT is accepted (migrate.ValidateMigrationCheckSQL: no
// PRAGMA — which could turn query_only back off — and no ATTACH, which
// creates files), on a connection of its own with PRAGMA query_only on, reset
// before the connection goes back to the pool (a connection whose reset fails
// is discarded; an in-memory database lives in its connection, so it is not
// discarded always). Bounded by migrate.CheckTimeout. A failure reports the
// SQLite result code only.
func (a *Applier) RunCheck(ctx context.Context, query, body string) (int64, error) {
	if err := migrate.ValidateMigrationCheckSQL(query, body, migrate.CheckSQLite); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, migrate.CheckTimeout)
	defer cancel()
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return 0, liteCheckErr(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return 0, liteCheckErr(err)
	}
	defer func() {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA query_only = OFF"); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	var n int64
	if err := conn.QueryRowContext(ctx, migrate.CheckCountSQL(query)).Scan(&n); err != nil {
		return 0, liteCheckErr(err)
	}
	return n, nil
}

// liteCheckErr reduces a check failure to its SQLite result code.
func liteCheckErr(err error) error {
	var se interface{ Code() int }
	if errors.As(err, &se) {
		return fmt.Errorf("the check failed (SQLite code %d)", se.Code())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("the check timed out")
	}
	return errors.New("the check failed")
}
