package fingerprint_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
)

// --- MySQL extractor ---------------------------------------------------
//
// ExtractMySQL takes the package's dbQuery interface (QueryContext).
// We back a fake with a real in-memory SQLite so the extractor sees
// genuine *sql.Rows — the MySQL-flavoured information_schema queries
// are intercepted and rewritten to equivalent SQLite reads over two
// fixture tables (t_tables, t_columns). This exercises the real scan
// + canonicalisation path without standing up MySQL.

type fakeMySQLDB struct {
	db           *sql.DB
	tablesQuery  string // SQLite query substituted for the tables read
	columnsQuery string // SQLite query substituted for the columns read
	tablesErr    error
	columnsErr   error
}

func (f *fakeMySQLDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "information_schema.tables") {
		if f.tablesErr != nil {
			return nil, f.tablesErr
		}
		q := f.tablesQuery
		if q == "" {
			q = `SELECT name FROM t_tables ORDER BY name`
		}
		return f.db.QueryContext(ctx, q)
	}
	if f.columnsErr != nil {
		return nil, f.columnsErr
	}
	q := f.columnsQuery
	if q == "" {
		q = `SELECT column_name, data_type, is_nullable, column_default
		     FROM t_columns WHERE table_name = ? ORDER BY column_name`
	}
	return f.db.QueryContext(ctx, q, args...)
}

func mysqlFixture(t *testing.T) *fakeMySQLDB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	exec := func(q string) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`CREATE TABLE t_tables (name TEXT)`)
	exec(`CREATE TABLE t_columns (table_name TEXT, column_name TEXT, data_type TEXT, is_nullable TEXT, column_default TEXT)`)
	exec(`INSERT INTO t_tables (name) VALUES ('users'), ('wc_migrations')`)
	exec(`INSERT INTO t_columns VALUES
		('users','id','bigint','NO',''),
		('users','email','varchar','NO',''),
		('users','nickname','varchar','YES','guest')`)
	return &fakeMySQLDB{db: db}
}

// TestExtractMySQL_RoundTrip — the MySQL extractor lists tables,
// skips the bookkeeping table, and canonicalises columns with
// nullability + defaults into the dialect-agnostic Format.
func TestExtractMySQL_RoundTrip(t *testing.T) {
	got, err := fingerprint.ExtractMySQL(context.Background(), mysqlFixture(t))
	if err != nil {
		t.Fatalf("ExtractMySQL: %v", err)
	}
	f := got.Format()
	if !strings.Contains(f, `TABLE "users"`) {
		t.Errorf("missing users table:\n%s", f)
	}
	if strings.Contains(f, "wc_migrations") {
		t.Errorf("wc_migrations must be excluded:\n%s", f)
	}
	if !strings.Contains(f, `COLUMN "email" "varchar" notnull`) {
		t.Errorf("email should be notnull:\n%s", f)
	}
	if !strings.Contains(f, `COLUMN "nickname" "varchar" null default="guest"`) {
		t.Errorf("nickname should be nullable w/ default:\n%s", f)
	}
}

// TestExtractMySQL_ListTablesError — a failing tables query
// surfaces as a wrapped "mysql list tables" error.
func TestExtractMySQL_ListTablesError(t *testing.T) {
	fx := mysqlFixture(t)
	fx.tablesErr = errors.New("boom")
	_, err := fingerprint.ExtractMySQL(context.Background(), fx)
	if err == nil || !strings.Contains(err.Error(), "mysql list tables") {
		t.Errorf("expected list-tables error, got %v", err)
	}
}

// TestExtractMySQL_ListColumnsError — tables read OK but the
// per-table columns read fails; wrapped "mysql list columns".
func TestExtractMySQL_ListColumnsError(t *testing.T) {
	fx := mysqlFixture(t)
	fx.columnsErr = errors.New("boom")
	_, err := fingerprint.ExtractMySQL(context.Background(), fx)
	if err == nil || !strings.Contains(err.Error(), "mysql list columns") {
		t.Errorf("expected list-columns error, got %v", err)
	}
}

// TestExtractMySQL_ScanTableError — a NULL table name can't scan
// into a string, surfacing the scan error branch.
func TestExtractMySQL_ScanTableError(t *testing.T) {
	fx := mysqlFixture(t)
	fx.tablesQuery = `SELECT NULL`
	_, err := fingerprint.ExtractMySQL(context.Background(), fx)
	if err == nil || !strings.Contains(err.Error(), "mysql scan table") {
		t.Errorf("expected scan-table error, got %v", err)
	}
}

// TestExtractMySQL_ScanColumnError — a column row with too few /
// NULL values fails the column scan.
func TestExtractMySQL_ScanColumnError(t *testing.T) {
	fx := mysqlFixture(t)
	fx.columnsQuery = `SELECT NULL, NULL, NULL, NULL`
	_, err := fingerprint.ExtractMySQL(context.Background(), fx)
	if err == nil || !strings.Contains(err.Error(), "mysql scan column") {
		t.Errorf("expected scan-column error, got %v", err)
	}
}

// --- Postgres extractor ------------------------------------------------
//
// ExtractPostgres takes a PgxQuerier returning pgx.Rows. We supply a
// hand-rolled fake of both so the extractor's scan + canonicalisation
// run without a live PG connection.

type fakePgRows struct {
	pgx.Rows
	rows [][]string
	idx  int
	err  error
}

func (r *fakePgRows) Next() bool {
	if r.err != nil {
		return false
	}
	r.idx++
	return r.idx <= len(r.rows)
}

func (r *fakePgRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	if len(dest) != len(row) {
		return errors.New("scan: column count mismatch")
	}
	for i := range dest {
		p, ok := dest[i].(*string)
		if !ok {
			return errors.New("scan: dest not *string")
		}
		*p = row[i]
	}
	return nil
}

func (r *fakePgRows) Err() error { return r.err }
func (r *fakePgRows) Close()     {}

type fakePgConn struct {
	tables     []string
	cols       map[string][][]string
	queryErr   error // fails every Query
	columnsErr error // fails only the columns Query
	tablesRows *fakePgRows
}

func (c *fakePgConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	if strings.Contains(sql, "information_schema.tables") {
		if c.tablesRows != nil {
			return c.tablesRows, nil
		}
		rows := make([][]string, len(c.tables))
		for i, t := range c.tables {
			rows[i] = []string{t}
		}
		return &fakePgRows{rows: rows}, nil
	}
	if c.columnsErr != nil {
		return nil, c.columnsErr
	}
	table, _ := args[0].(string)
	return &fakePgRows{rows: c.cols[table]}, nil
}

func pgFixture() *fakePgConn {
	return &fakePgConn{
		tables: []string{"users", "wc_migrations"},
		cols: map[string][][]string{
			"users": {
				{"id", "bigint", "NO", ""},
				{"email", "character varying", "NO", ""},
				{"nickname", "character varying", "YES", "'guest'::text"},
			},
		},
	}
}

// TestExtractPostgres_RoundTrip — lists public tables, skips the
// bookkeeping table, canonicalises columns identically to the
// other dialects' Format.
func TestExtractPostgres_RoundTrip(t *testing.T) {
	got, err := fingerprint.ExtractPostgres(context.Background(), pgFixture())
	if err != nil {
		t.Fatalf("ExtractPostgres: %v", err)
	}
	f := got.Format()
	if !strings.Contains(f, `TABLE "users"`) || strings.Contains(f, "wc_migrations") {
		t.Errorf("table set wrong:\n%s", f)
	}
	if !strings.Contains(f, `COLUMN "email" "character varying" notnull`) {
		t.Errorf("email notnull missing:\n%s", f)
	}
	if !strings.Contains(f, `COLUMN "nickname" "character varying" null default="'guest'::text"`) {
		t.Errorf("nickname null+default missing:\n%s", f)
	}
}

// TestExtractPostgres_CrossDialectFingerprintMatch — the canonical
// Format is dialect-agnostic, so a MySQL and a Postgres extraction
// of the same logical schema (modulo per-dialect type spelling)
// agree byte-for-byte when the type strings match. Here we pin that
// two PG extractions of identical metadata hash identically.
func TestExtractPostgres_Deterministic(t *testing.T) {
	a, _ := fingerprint.ExtractPostgres(context.Background(), pgFixture())
	b, _ := fingerprint.ExtractPostgres(context.Background(), pgFixture())
	if a.FingerprintHex() != b.FingerprintHex() {
		t.Errorf("identical PG metadata should hash identically:\n a=%s\n b=%s",
			a.FingerprintHex(), b.FingerprintHex())
	}
}

func TestExtractPostgres_ListTablesError(t *testing.T) {
	c := &fakePgConn{queryErr: errors.New("dial fail")}
	_, err := fingerprint.ExtractPostgres(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "postgres list tables") {
		t.Errorf("expected list-tables error, got %v", err)
	}
}

func TestExtractPostgres_ListColumnsError(t *testing.T) {
	c := pgFixture()
	c.columnsErr = errors.New("dial fail")
	_, err := fingerprint.ExtractPostgres(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "postgres list columns") {
		t.Errorf("expected list-columns error, got %v", err)
	}
}

// TestExtractPostgres_ScanTableError — a tables row whose Scan
// fails (wrong column count) surfaces the scan-table branch.
func TestExtractPostgres_ScanTableError(t *testing.T) {
	c := &fakePgConn{tablesRows: &fakePgRows{rows: [][]string{{"a", "b"}}}}
	_, err := fingerprint.ExtractPostgres(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "postgres scan table") {
		t.Errorf("expected scan-table error, got %v", err)
	}
}

// TestExtractPostgres_RowsErr — a tables iteration that ends with
// rows.Err() != nil propagates that error.
func TestExtractPostgres_RowsErr(t *testing.T) {
	c := &fakePgConn{tablesRows: &fakePgRows{err: errors.New("iteration broke")}}
	_, err := fingerprint.ExtractPostgres(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "iteration broke") {
		t.Errorf("expected rows.Err propagation, got %v", err)
	}
}

// TestExtractPostgres_ScanColumnError — a columns row with the
// wrong arity fails the column scan.
func TestExtractPostgres_ScanColumnError(t *testing.T) {
	c := &fakePgConn{
		tables: []string{"users"},
		cols:   map[string][][]string{"users": {{"only-one-col"}}},
	}
	_, err := fingerprint.ExtractPostgres(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "postgres scan column") {
		t.Errorf("expected scan-column error, got %v", err)
	}
}

// --- SQLite extractor error path --------------------------------------

// TestExtractSQLite_QueryError — a closed DB makes the tables
// query fail, surfacing the wrapped "sqlite list tables" error.
func TestExtractSQLite_QueryError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.Close() // queries now fail
	_, err = fingerprint.ExtractSQLite(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "sqlite list tables") {
		t.Errorf("expected list-tables error on closed DB, got %v", err)
	}
}
