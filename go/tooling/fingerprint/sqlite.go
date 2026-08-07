package fingerprint

import (
	"context"
	"fmt"
)

// ExtractSQLite queries sqlite_master + pragma_table_info for
// the canonical Schema. SQLite has no information_schema; the
// table-valued pragma functions are the equivalent. Excludes
// the `wc_migrations` bookkeeping table + SQLite's internal
// `sqlite_*` tables.
func ExtractSQLite(ctx context.Context, db dbQuery) (Schema, error) {
	tables, err := sqliteListTables(ctx, db)
	if err != nil {
		return Schema{}, err
	}
	out := Schema{Tables: make([]Table, 0, len(tables))}
	for _, name := range tables {
		cols, err := sqliteListColumns(ctx, db, name)
		if err != nil {
			return Schema{}, err
		}
		out.Tables = append(out.Tables, Table{Name: name, Columns: cols})
	}
	return out, nil
}

func sqliteListTables(ctx context.Context, db dbQuery) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table'
		  AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("sqlite list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite scan table: %w", err)
		}
		if shouldExclude(name) {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// sqliteListColumns uses pragma_table_info — a table-valued
// function exposed as a virtual table. Each row has
// (cid, name, type, notnull, dflt_value, pk).
func sqliteListColumns(ctx context.Context, db dbQuery, table string) ([]Column, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name, type, "notnull", COALESCE(dflt_value, '') FROM pragma_table_info(?)`,
		table)
	if err != nil {
		return nil, fmt.Errorf("sqlite list columns %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Column
	for rows.Next() {
		var name, dtype, def string
		var notnull int
		if err := rows.Scan(&name, &dtype, &notnull, &def); err != nil {
			return nil, fmt.Errorf("sqlite scan column %s: %w", table, err)
		}
		out = append(out, Column{
			Name:     name,
			DataType: dtype,
			Nullable: notnull == 0,
			Default:  def,
		})
	}
	return out, rows.Err()
}
