// Observation — the live schema reported as STRUCTURE rather than as a hash.
//
// The fingerprint answers "has this changed"; an observation answers "what is
// in there". They read the same database and must not read it twice in two
// ways, so the extraction below is the one the console is given and the
// fingerprint stays exactly what it was — a hash over the `public` schema,
// unchanged, because stored fingerprints were computed that way and a
// different input silently invalidates every one of them.
package fingerprint

import (
	"context"
	"fmt"
)

// Observed is one database's live schema, namespaces included.
//
// Namespaces are the reason this is not just Schema: the fingerprint scopes to
// `public`, so a table a (w17.module).schema put in `billing` is invisible to
// it. For a hash that is a deliberate narrowing; for an observation it would
// be a lie, and a comparison against it would report every namespaced table as
// missing.
type Observed struct {
	Tables []ObservedTable
}

// ObservedTable is one table, identified by the pair that actually names it.
type ObservedTable struct {
	Schema  string
	Name    string
	Columns []Column
}

// ObservePostgres reads every table a connection can see, in every namespace
// except the server's own.
//
// Deliberately dumb: names and columns as the database spells them, no
// mapping to w17 types. The client reporting this must not have to understand
// a schema, and the console — which owns every rule about what a difference
// means — already does.
func ObservePostgres(ctx context.Context, conn PgxQuerier) (Observed, error) {
	rows, err := conn.Query(ctx, `
		SELECT table_schema, table_name
		FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		  AND table_type = 'BASE TABLE'`)
	if err != nil {
		return Observed{}, fmt.Errorf("postgres observe tables: %w", err)
	}
	type ref struct{ schema, name string }
	var refs []ref
	for rows.Next() {
		var s, n string
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return Observed{}, fmt.Errorf("postgres observe scan: %w", err)
		}
		if shouldExclude(n) {
			continue
		}
		refs = append(refs, ref{s, n})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Observed{}, fmt.Errorf("postgres observe tables: %w", err)
	}

	out := Observed{Tables: make([]ObservedTable, 0, len(refs))}
	for _, r := range refs {
		cols, err := pgObserveColumns(ctx, conn, r.schema, r.name)
		if err != nil {
			return Observed{}, err
		}
		out.Tables = append(out.Tables, ObservedTable{Schema: r.schema, Name: r.name, Columns: cols})
	}
	return out, nil
}

func pgObserveColumns(ctx context.Context, conn PgxQuerier, schema, table string) ([]Column, error) {
	rows, err := conn.Query(ctx, `
		SELECT column_name, data_type, is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2`,
		schema, table)
	if err != nil {
		return nil, fmt.Errorf("postgres observe columns %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var name, dtype, isNullable, def string
		if err := rows.Scan(&name, &dtype, &isNullable, &def); err != nil {
			return nil, fmt.Errorf("postgres observe scan column %s.%s: %w", schema, table, err)
		}
		out = append(out, Column{Name: name, DataType: dtype, Nullable: isNullable == "YES", Default: def})
	}
	return out, rows.Err()
}
