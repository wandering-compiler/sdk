package fingerprint_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
)

// fakeSQLiteDB satisfies the package's dbQuery interface, backed by a real
// in-memory SQLite over fixture tables. Per-query substitutions + injected
// errors drive ExtractSQLite's error arms without standing up the real
// sqlite_master / pragma_table_info reads.
type fakeSQLiteDB struct {
	db           *sql.DB
	tablesQuery  string // substituted for the sqlite_master read
	columnsQuery string // substituted for the pragma_table_info read
	columnsErr   error
}

func (f *fakeSQLiteDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "sqlite_master") {
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
		q = `SELECT name, type, "notnull", dflt FROM t_cols WHERE tbl = ? ORDER BY name`
	}
	return f.db.QueryContext(ctx, q, args...)
}

func sqliteFixture(t *testing.T) *fakeSQLiteDB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE t_tables (name TEXT)`,
		`CREATE TABLE t_cols (tbl TEXT, name TEXT, type TEXT, "notnull" INT, dflt TEXT)`,
		`INSERT INTO t_tables (name) VALUES ('users')`,
		`INSERT INTO t_cols VALUES ('users','id','INTEGER',1,'')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return &fakeSQLiteDB{db: db}
}

// TestSchemaFormat_SortsMultipleTables covers the table-sort comparator (it
// only fires with ≥2 tables) and the nullable + default column branches.
func TestSchemaFormat_SortsMultipleTables(t *testing.T) {
	s := fingerprint.Schema{Tables: []fingerprint.Table{
		{Name: "zebra", Columns: []fingerprint.Column{{Name: "x", DataType: "TEXT", Nullable: true}}},
		{Name: "alpha", Columns: []fingerprint.Column{{Name: "y", DataType: "INT", Default: "0"}}},
	}}
	out := s.Format()
	ai := strings.Index(out, `TABLE "alpha"`)
	zi := strings.Index(out, `TABLE "zebra"`)
	if ai < 0 || zi < 0 || ai > zi {
		t.Errorf("tables not sorted alpha<zebra:\n%s", out)
	}
	if !strings.Contains(out, `COLUMN "x" "TEXT" null`) {
		t.Errorf("nullable column not formatted as null:\n%s", out)
	}
	if !strings.Contains(out, `default="0"`) {
		t.Errorf("default not formatted:\n%s", out)
	}
}

// TestExtractSQLite_ColumnsQueryError covers sqliteListColumns' query-error arm
// AND ExtractSQLite's propagation of it (tables read fine, columns read fails).
func TestExtractSQLite_ColumnsQueryError(t *testing.T) {
	f := sqliteFixture(t)
	f.columnsErr = errors.New("boom")
	if _, err := fingerprint.ExtractSQLite(context.Background(), f); err == nil ||
		!strings.Contains(err.Error(), "sqlite list columns") {
		t.Errorf("expected list-columns error, got %v", err)
	}
}

// TestExtractSQLite_TableScanError covers sqliteListTables' scan-error arm: the
// substituted query returns two columns, so Scan(&name) fails.
func TestExtractSQLite_TableScanError(t *testing.T) {
	f := sqliteFixture(t)
	f.tablesQuery = `SELECT name, name FROM t_tables` // 2 cols → scan mismatch
	if _, err := fingerprint.ExtractSQLite(context.Background(), f); err == nil ||
		!strings.Contains(err.Error(), "sqlite scan table") {
		t.Errorf("expected scan-table error, got %v", err)
	}
}

// TestExtractSQLite_ColumnScanError covers sqliteListColumns' scan-error arm:
// the substituted query yields a non-integer in the notnull position.
func TestExtractSQLite_ColumnScanError(t *testing.T) {
	f := sqliteFixture(t)
	f.columnsQuery = `SELECT 'id', 'INTEGER', 'not-an-int', ''` // notnull→int scan fails
	if _, err := fingerprint.ExtractSQLite(context.Background(), f); err == nil ||
		!strings.Contains(err.Error(), "sqlite scan column") {
		t.Errorf("expected scan-column error, got %v", err)
	}
}
