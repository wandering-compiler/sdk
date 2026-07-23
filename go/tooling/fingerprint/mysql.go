package fingerprint

import (
	"context"
	"fmt"
)

// ExtractMySQL queries information_schema on a MySQL connection
// and returns the canonical Schema for fingerprinting. Scopes to
// the current database (from `DATABASE()`); the deploy client
// connects with the database in the DSN, so DATABASE() returns
// the right value at apply time. Excludes the `wc_migrations`
// bookkeeping table.
func ExtractMySQL(ctx context.Context, db dbQuery) (Schema, error) {
	tables, err := mysqlListTables(ctx, db)
	if err != nil {
		return Schema{}, err
	}
	out := Schema{Tables: make([]Table, 0, len(tables))}
	for _, name := range tables {
		cols, err := mysqlListColumns(ctx, db, name)
		if err != nil {
			return Schema{}, err
		}
		out.Tables = append(out.Tables, Table{Name: name, Columns: cols})
	}
	return out, nil
}

func mysqlListTables(ctx context.Context, db dbQuery) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = DATABASE()
		  AND table_type = 'BASE TABLE'`)
	if err != nil {
		return nil, fmt.Errorf("mysql list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("mysql scan table: %w", err)
		}
		if shouldExclude(name) {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func mysqlListColumns(ctx context.Context, db dbQuery, table string) ([]Column, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ?`,
		table)
	if err != nil {
		return nil, fmt.Errorf("mysql list columns %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Column
	for rows.Next() {
		var name, dtype, isNullable, def string
		if err := rows.Scan(&name, &dtype, &isNullable, &def); err != nil {
			return nil, fmt.Errorf("mysql scan column %s: %w", table, err)
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
