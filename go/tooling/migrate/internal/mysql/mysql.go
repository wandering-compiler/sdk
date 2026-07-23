// Package mysql is the production-side MySQL Applier for
// w17ctl. Connects via `github.com/go-sql-driver/mysql`
// through `database/sql`. The driver requires its own DSN
// format (`user:pass@tcp(host:port)/db?...`) — w17ctl
// accepts URL-shaped DSNs (`mysql://user:pass@host:port/db?...`)
// for cross-dialect uniformity, so this package converts at New
// time.
//
// Multi-statement DDL: MySQL DDL implicitly commits any open
// transaction, so the BEGIN / COMMIT framing the migrator emits
// is informational. We force `multiStatements=true` on the
// resolved DSN (overriding the user's setting if present) so
// migrator's bodies execute in a single Exec call.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	_ "github.com/go-sql-driver/mysql"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Applier is the per-connection MySQL apply driver.
type Applier struct {
	db *sql.DB
}

var _ migrate.Applier = (*Applier)(nil)
var _ migrate.Wiper = (*Applier)(nil)
var _ migrate.FingerprintCapable = (*Applier)(nil)

// New opens a database/sql connection to the supplied URL DSN.
// Eagerly Pings the DB so DSN typos / connection failures
// surface at New time rather than on the first Apply.
func New(ctx context.Context, urlDSN string) (*Applier, error) {
	if urlDSN == "" {
		return nil, fmt.Errorf("mysql.New: dsn is empty")
	}
	driverDSN, err := URLToDriverDSN(urlDSN)
	if err != nil {
		return nil, fmt.Errorf("mysql.New: %w", err)
	}
	db, err := sql.Open("mysql", driverDSN)
	if err != nil {
		return nil, fmt.Errorf("mysql.New: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql.New: ping: %w", err)
	}
	return &Applier{db: db}, nil
}

// AppliedHead returns the id of the most recently applied
// migration on this DB by querying `wc_migrations` (D27).
// Missing table = empty string (treated as fresh DB by the
// orchestrator). MySQL surfaces "Error 1146 (42S02): Table
// '<schema>.wc_migrations' doesn't exist" — we sniff the SQLState
// 42S02 (base table or view not found) for the missing-table
// case.
func (a *Applier) AppliedHead(ctx context.Context) (string, error) {
	var head sql.NullString
	err := a.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(timestamp), '') FROM wc_migrations`,
	).Scan(&head)
	if err != nil {
		// MySQL "table doesn't exist" — fresh DB.
		if isMissingTable(err) {
			return "", nil
		}
		return "", fmt.Errorf("mysql AppliedHead: %w", err)
	}
	return head.String, nil
}

// isMissingTable sniffs go-sql-driver's "table doesn't exist"
// error. The driver wraps the server's MYSQL_ERROR with a Number
// field; 1146 = ER_NO_SUCH_TABLE. Falls back to substring match
// for resilience against driver internals changes.
func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1146") ||
		strings.Contains(msg, "doesn't exist") ||
		strings.Contains(msg, "Unknown table")
}

// Apply executes the migration's up_sql + up_post_tx. Both go
// through go-sql-driver's multi-statement path because we forced
// `multiStatements=true` in the DSN.
//
// RESUME-GAP INVARIANT (Q52): unlike Postgres, MySQL has no
// PhasePending recovery here — AppliedHead does not filter on a
// `post_tx_complete` marker, and up_post_tx runs as a SEPARATE Exec
// after up_sql has already committed. In the skirt layout the
// wc_migrations bookkeeping row is written at the END of up_post_tx
// (applied.Wrap's legacy-skirt path), NOT in up_sql. So a crash BETWEEN
// the two Execs leaves up_sql's DDL committed with NO applied-state row:
// a re-run sees the migration as un-applied (AppliedHead misses it) and
// re-executes up_sql, which then WEDGES LOUDLY on the already-committed
// DDL ("… already exists"). This is tolerable only because up_post_tx is
// normally empty for MySQL: the migrator routes ops into the post-tx
// bucket solely when an index op carries non_transactional=true (= a
// CONCURRENTLY index), a Postgres-only construct — MySQL builds indexes
// online with no such syntax. When a non-empty up_post_tx does reach this
// dialect we emit a WARNING noting the non-resumable gap.
func (a *Applier) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	// ZERO-SQL-RECORD belt — an empty up_sql (e.g. --no-applied-state on an empty
	// migration) would make MySQL's empty COM_QUERY abort with ER 1065; skip the
	// Exec (up_post_tx, if any, still runs below).
	if up := m.GetUpSql(); up != "" {
		if _, err := a.db.ExecContext(ctx, up); err != nil {
			return fmt.Errorf("mysql apply up_sql: %w", err)
		}
	}
	if pt := m.GetUpPostTx(); pt != "" {
		slog.Warn("mysql: running non-empty up_post_tx as a separate step after up_sql committed; a crash between them is NOT resumable — up_sql's DDL commits with no applied-state row (recorded at the end of up_post_tx), so a re-run re-executes up_sql and wedges on the already-committed DDL",
			slog.String("migration_id", m.GetId()))
		if _, err := a.db.ExecContext(ctx, pt); err != nil {
			return fmt.Errorf("mysql apply up_post_tx: %w", err)
		}
	}
	return nil
}

// Rollback runs the migration's down payload. Order:
// down_pre_tx first (post-tx-equivalent), then down_sql (in-tx
// body including the wc_migrations DELETE).
// multiStatements=true is forced in URLToDriverDSN, so each
// body executes in a single ExecContext call.
func (a *Applier) Rollback(ctx context.Context, m *applyfetchpb.Migration) error {
	if pre := m.GetDownPreTx(); pre != "" {
		if _, err := a.db.ExecContext(ctx, pre); err != nil {
			return fmt.Errorf("mysql rollback down_pre_tx: %w", err)
		}
	}
	if down := m.GetDownSql(); down != "" {
		if _, err := a.db.ExecContext(ctx, down); err != nil {
			return fmt.Errorf("mysql rollback down_sql: %w", err)
		}
	}
	return nil
}

// Wipe drops every table in the connected database (migrate.Wiper) — the
// dev fresh-build primitive. Disables FK checks, enumerates the current
// DATABASE()'s tables via information_schema, DROPs each, then restores
// FK checks. Only the connected schema is touched.
func (a *Applier) Wipe(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0"); err != nil {
		return fmt.Errorf("mysql Wipe: disable FK checks: %w", err)
	}
	// List tables in a closure so `defer rows.Close()` releases the cursor
	// BEFORE the DROP loop (and rows.Err() is checked) — the rows must be
	// closed before the writes, not at function end.
	listTables := func() ([]string, error) {
		rows, err := a.db.QueryContext(ctx,
			"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()")
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
		return fmt.Errorf("mysql Wipe: list tables: %w", err)
	}
	for _, n := range names {
		if _, err := a.db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+n+"`"); err != nil {
			return fmt.Errorf("mysql Wipe: drop %s: %w", n, err)
		}
	}
	if _, err := a.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1"); err != nil {
		return fmt.Errorf("mysql Wipe: restore FK checks: %w", err)
	}
	return nil
}

// Close releases the underlying sql.DB pool. Idempotent — second
// Close on a closed DB returns nil.
func (a *Applier) Close() error {
	if a.db == nil {
		return nil
	}
	err := a.db.Close()
	a.db = nil
	return err
}

// Fingerprint extracts the canonical MySQL schema state via
// information_schema and returns its hex-encoded sha256
// (Phase D — D-iter3-14). Excludes the wc_migrations
// bookkeeping table; sorted by name + columns. Schema scope is
// the current `DATABASE()` (= the DB embedded in the DSN).
func (a *Applier) Fingerprint(ctx context.Context) (string, error) {
	schema, err := fingerprint.ExtractMySQL(ctx, a.db)
	if err != nil {
		return "", err
	}
	return schema.FingerprintHex(), nil
}

// URLToDriverDSN converts `mysql://user:pass@host:port/db?params`
// to the go-sql-driver/mysql DSN format. Forces
// `multiStatements=true` so multi-statement DDL bodies the
// migrator emits go through in one Exec call.
//
// Exposed (not unexported) so tests can pin the conversion
// without going through New (which dials).
func URLToDriverDSN(urlDSN string) (string, error) {
	u, err := url.Parse(urlDSN)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "mysql" {
		return "", fmt.Errorf("expected mysql:// scheme, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host (expected user:pass@host:port)")
	}

	// User info — optional. go-sql-driver accepts an empty user.
	userInfo := ""
	if u.User != nil {
		userInfo = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			userInfo += ":" + pw
		}
		userInfo += "@"
	}

	// DB — strip the leading slash from u.Path. Empty DB is a
	// legal DSN (server-level connection).
	db := strings.TrimPrefix(u.Path, "/")

	// Query params — force multiStatements=true. Override user's
	// setting if explicitly disabled (the migrator's bodies
	// require it).
	q := u.Query()
	q.Set("multiStatements", "true")
	params := q.Encode()
	suffix := ""
	if params != "" {
		suffix = "?" + params
	}

	return fmt.Sprintf("%stcp(%s)/%s%s", userInfo, u.Host, db, suffix), nil
}
