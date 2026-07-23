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
	"fmt"
	"log/slog"
	"strings"

	_ "modernc.org/sqlite"

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
	// F7-A-5 / WOB3: register the W17_UNICODE collation before opening, so
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
// migration on this DB by querying `wc_migrations` (D27).
// Missing table = empty string (treated as fresh DB by the
// orchestrator). SQLite returns "no such table: wc_migrations"
// on absence, sniffed via substring match on the error message
// (modernc.org/sqlite does not expose typed error codes).
func (a *Applier) AppliedHead(ctx context.Context) (string, error) {
	var head sql.NullString
	err := a.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(timestamp), '') FROM wc_migrations`,
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
// RESUME-GAP INVARIANT (Q52): unlike Postgres, SQLite has no
// PhasePending recovery here — AppliedHead does not filter on a
// `post_tx_complete` marker, and up_post_tx runs as a SEPARATE Exec
// after up_sql has already committed. In the skirt layout the
// wc_migrations bookkeeping row is written at the END of up_post_tx
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
	if err := assertNoForeignKeyViolations(ctx, conn, m.GetUpSql(), m.GetId()); err != nil {
		return err
	}
	return nil
}

// assertNoForeignKeyViolations runs `PRAGMA foreign_key_check` as a query and
// returns an error if the database holds any FK violation. It only fires for a
// rebuild migration — detected by the `PRAGMA foreign_key_check` marker the
// emit body embeds for the 12-step recipe (a non-rebuild migration enforces FKs
// inline under `foreign_keys=ON`, so there is nothing extra to verify and no
// reason to pay for the scan). foreign_key_check reports one row per violation
// (child table, rowid, parent table, fk id); a non-empty result is a failure.
func assertNoForeignKeyViolations(ctx context.Context, conn *sql.Conn, upSQL, migID string) error {
	if !strings.Contains(upSQL, "PRAGMA foreign_key_check") {
		return nil
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check;")
	if err != nil {
		return fmt.Errorf("sqlite apply: PRAGMA foreign_key_check (migration %s): %w", migID, err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return fmt.Errorf("sqlite apply: migration %s introduced foreign-key violation(s) — the rebuild ran under foreign_keys=OFF and PRAGMA foreign_key_check found dangling reference(s); the schema change is committed but the data is now inconsistent (repair the offending rows, or restore and re-run against clean data)", migID)
	}
	return rows.Err()
}

// Rollback runs the migration's down payload. Order:
// down_pre_tx first, then down_sql (in-tx body including the
// wc_migrations DELETE applied.Wrap injected).
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
	if err := assertNoForeignKeyViolations(ctx, conn, m.GetDownSql()+m.GetDownPreTx(), m.GetId()); err != nil {
		return err
	}
	return nil
}

// Wipe drops every user table (migrate.Wiper) — the dev fresh-build
// primitive. Enumerates sqlite_master (skipping SQLite's internal
// sqlite_* objects) and DROPs each table with foreign_keys off so
// dependency order doesn't matter.
func (a *Applier) Wipe(ctx context.Context) error {
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
// sha256 (Phase D — D-iter3-14). Excludes the wc_migrations
// bookkeeping table + SQLite's internal sqlite_* tables; sorted
// by name + columns.
func (a *Applier) Fingerprint(ctx context.Context) (string, error) {
	schema, err := fingerprint.ExtractSQLite(ctx, a.db)
	if err != nil {
		return "", err
	}
	return schema.FingerprintHex(), nil
}

// URLToDriverDSN converts the w17ctl-accepted DSN forms
// into the form modernc.org/sqlite expects. Exposed for tests.
//
//   - `sqlite:///abs/path.db` → `/abs/path.db`
//   - `sqlite://relative.db`  → `relative.db`
//   - `file:relative.db?…`    → unchanged (driver-native)
//   - anything else           → unchanged (pass through; driver
//     surfaces a clear error if the form is wrong)
func URLToDriverDSN(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "sqlite:///"):
		return dsn[len("sqlite://"):] // keep leading "/"
	case strings.HasPrefix(dsn, "sqlite://"):
		return dsn[len("sqlite://"):]
	}
	return dsn
}
