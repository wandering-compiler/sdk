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
	// Indexes are the table's own — the ones a CREATE INDEX made. The
	// index a primary key or a unique CONSTRAINT creates underneath itself
	// is left out: it belongs to the constraint, no migration names it, and
	// reported here it would read as an index the schema never declared and
	// be planned for dropping.
	Indexes []ObservedIndex
	// PrimaryKey is the key's columns in key order, as the database spells
	// them.
	PrimaryKey []string
	// ForeignKeys are the table's FK constraints.
	ForeignKeys []ObservedForeignKey
	// Checks are the table's CHECK constraints, by name.
	Checks []string
}

// ObservedForeignKey is one FK constraint: the column it constrains and what
// it points at. Single-column only, which is what w17 emits; a composite one
// reports its first column and is compared on that.
type ObservedForeignKey struct {
	Name string
	// Columns are every column the key constrains, in the database's order.
	// Plural because a scope-preserving key constrains two, and the pair —
	// not either one alone — is what identifies it against a declaration.
	Columns      []string
	TargetTable  string
	TargetColumn string
}

// ObservedIndex is one index as the database defines it.
//
// Definition rather than parts: `pg_get_indexdef` renders the whole statement,
// expressions and partial predicate included, which is the only form that
// survives an index this reader does not model. It is carried so a comparison
// can be made at all; nothing here parses it.
type ObservedIndex struct {
	Name       string
	Unique     bool
	Definition string
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
		idx, pk, err := pgObserveIndexes(ctx, conn, r.schema, r.name)
		if err != nil {
			return Observed{}, err
		}
		fks, checks, err := pgObserveConstraints(ctx, conn, r.schema, r.name)
		if err != nil {
			return Observed{}, err
		}
		out.Tables = append(out.Tables, ObservedTable{
			Schema: r.schema, Name: r.name, Columns: cols, Indexes: idx, PrimaryKey: pk,
			ForeignKeys: fks, Checks: checks,
		})
	}
	return out, nil
}

// pgObserveIndexes reads a table's indexes and its primary key.
//
// One query for both because they come from the same catalogue row: a primary
// key IS an index in Postgres, flagged, and reading them separately invites
// the two to disagree about a table that changed between the reads.
//
// Constraint-backed indexes are skipped. The index under a PRIMARY KEY or a
// UNIQUE constraint is created by the constraint and dropped with it; no
// migration ever names one, so reported as an index it reads as something the
// schema never declared and gets planned for a DROP that would fail.
func pgObserveIndexes(ctx context.Context, conn PgxQuerier, schema, table string) ([]ObservedIndex, []string, error) {
	rows, err := conn.Query(ctx, `
		SELECT ic.relname,
		       i.indisunique,
		       i.indisprimary,
		       pg_get_indexdef(i.indexrelid),
		       COALESCE((SELECT array_agg(a.attname ORDER BY k.ord)
		                   FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
		                   JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum), '{}')
		  FROM pg_index i
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		  JOIN pg_class tc ON tc.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = tc.relnamespace
		 WHERE n.nspname = $1 AND tc.relname = $2
		   AND NOT EXISTS (
		         SELECT 1 FROM pg_constraint c
		          WHERE c.conindid = i.indexrelid AND c.contype IN ('p', 'u', 'x'))
		 ORDER BY ic.relname`,
		schema, table)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres observe indexes %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []ObservedIndex
	for rows.Next() {
		var name, def string
		var unique, primary bool
		var cols []string
		if err := rows.Scan(&name, &unique, &primary, &def, &cols); err != nil {
			return nil, nil, fmt.Errorf("postgres observe scan index %s.%s: %w", schema, table, err)
		}
		out = append(out, ObservedIndex{Name: name, Unique: unique, Definition: def})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("postgres observe indexes %s.%s: %w", schema, table, err)
	}

	pk, err := pgObservePrimaryKey(ctx, conn, schema, table)
	if err != nil {
		return nil, nil, err
	}
	return out, pk, nil
}

// pgObservePrimaryKey reads the primary key's columns, in key order.
func pgObservePrimaryKey(ctx context.Context, conn PgxQuerier, schema, table string) ([]string, error) {
	rows, err := conn.Query(ctx, `
		SELECT a.attname
		  FROM pg_index i
		  JOIN pg_class tc ON tc.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = tc.relnamespace
		  JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		 WHERE n.nspname = $1 AND tc.relname = $2 AND i.indisprimary
		 ORDER BY k.ord`,
		schema, table)
	if err != nil {
		return nil, fmt.Errorf("postgres observe primary key %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("postgres observe scan primary key %s.%s: %w", schema, table, err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func pgObserveColumns(ctx context.Context, conn PgxQuerier, schema, table string) ([]Column, error) {
	// format_type rather than information_schema.data_type, and the reason is
	// what the caller does with the answer.
	//
	// information_schema reports `character varying` and puts the length in a
	// second column, `ARRAY` for every array whatever its element, and
	// `USER-DEFINED` for an enum, a domain and a PostGIS geography alike. A
	// comparison against a declared type cannot be made out of that without
	// reassembling the spelling by hand, per type, in the reader.
	//
	// format_type is the database's own canonical spelling — `character
	// varying(64)`, `numeric(12,2)`, `text[]`, `geography(Point,4326)` — which
	// is the same thing the emitter renders, so the two can simply be
	// compared.
	rows, err := conn.Query(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE n.nspname = $1 AND c.relname = $2
		   AND a.attnum > 0 AND NOT a.attisdropped
		 ORDER BY a.attnum`,
		schema, table)
	if err != nil {
		return nil, fmt.Errorf("postgres observe columns %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var name, dtype, def string
		var nullable bool
		if err := rows.Scan(&name, &dtype, &nullable, &def); err != nil {
			return nil, fmt.Errorf("postgres observe scan column %s.%s: %w", schema, table, err)
		}
		out = append(out, Column{Name: name, DataType: dtype, Nullable: nullable, Default: def})
	}
	return out, rows.Err()
}

// pgObserveConstraints reads a table's foreign keys and the NAMES of its check
// constraints.
//
// Names rather than expressions for the checks, and that is a deliberate
// narrowing: Postgres rewrites a CHECK body (`((length(title) <= 200))`), so
// comparing expressions would report a change on every check ever written.
// The name is derived from the column and the kind of check, so it answers the
// question a sync actually has — is this check here or not.
//
// NOT VALID constraints are reported like any other: they exist, and a sync
// that planned to re-add one would fail on the duplicate name.
func pgObserveConstraints(ctx context.Context, conn PgxQuerier, schema, table string) ([]ObservedForeignKey, []string, error) {
	rows, err := conn.Query(ctx, `
		SELECT c.conname,
		       c.contype,
		       COALESCE((SELECT array_agg(a.attname ORDER BY k.ord)
		                   FROM unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
		                   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum), '{}'),
		       COALESCE(ft.relname, ''),
		       COALESCE((SELECT a.attname FROM pg_attribute a
		                  WHERE a.attrelid = c.confrelid AND a.attnum = c.confkey[1]), '')
		  FROM pg_constraint c
		  JOIN pg_class tc ON tc.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = tc.relnamespace
		  LEFT JOIN pg_class ft ON ft.oid = c.confrelid
		 WHERE n.nspname = $1 AND tc.relname = $2 AND c.contype IN ('f', 'c')
		 ORDER BY c.conname`,
		schema, table)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres observe constraints %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var fks []ObservedForeignKey
	var checks []string
	for rows.Next() {
		var name, target, targetCol string
		var cols []string
		var kind byte
		if err := rows.Scan(&name, &kind, &cols, &target, &targetCol); err != nil {
			return nil, nil, fmt.Errorf("postgres observe scan constraint %s.%s: %w", schema, table, err)
		}
		switch kind {
		case 'f':
			fks = append(fks, ObservedForeignKey{Name: name, Columns: cols, TargetTable: target, TargetColumn: targetCol})
		case 'c':
			checks = append(checks, name)
		}
	}
	return fks, checks, rows.Err()
}
