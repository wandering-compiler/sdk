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
// except the server's own and except the ones an EXTENSION owns.
//
// Deliberately dumb: names and columns as the database spells them, no
// mapping to w17 types. The client reporting this must not have to understand
// a schema, and the console — which owns every rule about what a difference
// means — already does.
//
// Extension-owned tables are not dumb-read, and that is the one rule here.
// `CREATE EXTENSION postgis` on the postgis/postgis image brings 36 tables
// with it — `spatial_ref_sys`, all of `tiger.*`, `topology.*` — none of which
// any checkpoint created or could have created. Reported, they read as a
// drifted database and a dev build REFUSES to plan against the very image a
// geospatial project would obviously use. They are also not the client's to
// report in the first place: the extension owns them, `DROP EXTENSION` takes
// them away, and no migration will ever mention one.
//
// So the read moves off information_schema, which cannot express ownership,
// onto pg_class + pg_depend, which can. Both directions are excluded: a table
// that is itself an extension member, and any table sitting inside a schema
// the extension created (tiger's own staging tables are the case — created by
// the extension's scripts, in the extension's schema, members of nothing).
func ObservePostgres(ctx context.Context, conn PgxQuerier) (Observed, error) {
	rows, err := conn.Query(ctx, `
		SELECT n.nspname, c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg\_%'
		  AND NOT EXISTS (
		        SELECT 1 FROM pg_depend d
		        WHERE d.classid = 'pg_class'::regclass
		          AND d.objid = c.oid AND d.deptype = 'e')
		  AND NOT EXISTS (
		        SELECT 1 FROM pg_depend d
		        WHERE d.classid = 'pg_namespace'::regclass
		          AND d.objid = n.oid AND d.deptype = 'e')`)
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
