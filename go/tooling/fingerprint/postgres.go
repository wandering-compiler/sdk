package fingerprint

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PgxQuerier is the slice of pgx.Conn the postgres extractor
// needs. Defined as an interface so tests can substitute a
// mock without standing up a real pgx.Conn.
type PgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ExtractPostgres queries the catalog on a PG connection and returns the
// canonical Schema for fingerprinting. Scopes to the `public` schema
// (matches the deploy-time convention — w17migrate connects with no
// search_path override). Excludes the `w17_migrations` bookkeeping table,
// and excludes what an EXTENSION owns.
//
// The extension exclusion is what lets "empty" stay a usable word. PostGIS
// installs `spatial_ref_sys` into `public`, so a database that has the
// extension and nothing else hashes as POPULATED — and the one caller that
// asks (`schema apply`, which builds from empty) then declines to build,
// leaving a database with no tables and a seed step that fails on the first
// INSERT. Measured, not reasoned: that is what the pg-native stack did the
// moment its postgres image became a postgis one.
//
// It does not change any fingerprint computed before it. The exclusion can
// only drop extension-owned tables, no w17 database had any until geometric
// columns existed, and the tables it drops were never the schema this hash
// is about: DROP EXTENSION takes them away and no migration mentions one.
//
// Takes a `*pgx.Conn` (or a test-fitting PgxQuerier) because the
// production PG Applier uses pgx, not database/sql.
func ExtractPostgres(ctx context.Context, conn PgxQuerier) (Schema, error) {
	tables, err := pgListTables(ctx, conn)
	if err != nil {
		return Schema{}, err
	}
	out := Schema{Tables: make([]Table, 0, len(tables))}
	for _, name := range tables {
		cols, err := pgListColumns(ctx, conn, name)
		if err != nil {
			return Schema{}, err
		}
		out.Tables = append(out.Tables, Table{Name: name, Columns: cols})
	}
	return out, nil
}

func pgListTables(ctx context.Context, conn PgxQuerier) ([]string, error) {
	rows, err := conn.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind IN ('r', 'p')
		  AND NOT EXISTS (
		        SELECT 1 FROM pg_depend d
		        WHERE d.classid = 'pg_class'::regclass
		          AND d.objid = c.oid AND d.deptype = 'e')`)
	if err != nil {
		return nil, fmt.Errorf("postgres list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("postgres scan table: %w", err)
		}
		if shouldExclude(name) {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func pgListColumns(ctx context.Context, conn PgxQuerier, table string) ([]Column, error) {
	rows, err := conn.Query(ctx, `
		SELECT column_name, data_type, is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1`,
		table)
	if err != nil {
		return nil, fmt.Errorf("postgres list columns %s: %w", table, err)
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var name, dtype, isNullable, def string
		if err := rows.Scan(&name, &dtype, &isNullable, &def); err != nil {
			return nil, fmt.Errorf("postgres scan column %s: %w", table, err)
		}
		out = append(out, Column{
			Name:     name,
			DataType: dtype,
			Nullable: isNullable == "YES",
			Default:  def,
		})
	}
	return out, rows.Err()
}
