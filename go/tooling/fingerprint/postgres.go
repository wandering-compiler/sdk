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

// ExtractPostgres queries information_schema on a PG connection
// and returns the canonical Schema for fingerprinting. Scopes
// to the `public` schema (matches the deploy-time convention —
// w17migrate connects with no search_path override). Excludes
// the `wc_migrations` bookkeeping table.
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
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'public'
		  AND table_type = 'BASE TABLE'`)
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
