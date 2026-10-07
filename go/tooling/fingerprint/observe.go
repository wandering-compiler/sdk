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
	"strings"
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
	// EnumTypes are the database's native ENUM types with their labels. A
	// column only NAMES its type; the labels — what the database will
	// actually accept — live here, and a column is tied to its entry by
	// spelling (ObservedEnumType.Type == Column.DataType).
	EnumTypes []ObservedEnumType
}

// ObservedEnumType is one native enum type: where it lives, how a column of
// it spells its type, and the labels in the database's sort order.
type ObservedEnumType struct {
	Schema string
	Name   string
	// Type is `format_type(oid, NULL)` — the exact string a column of this
	// type reports as its DataType on the same connection, qualified when the
	// type is not on the reader's search_path.
	Type string
	// Labels in `enumsortorder`: the order is part of the type.
	Labels []string
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
	// PrimaryKeyName is the key CONSTRAINT's name. Not derivable: Postgres
	// names an inline key `<table>_pkey`, but a hand-made table can name it
	// anything and a renamed table keeps its old `<old>_pkey` — and a key
	// change that drops a derived name that does not exist fails its ADD
	// PRIMARY KEY with "multiple primary keys" (pass #49 B49-6). Empty for
	// a table with no key.
	PrimaryKeyName string
	// ForeignKeys are the table's FK constraints.
	ForeignKeys []ObservedForeignKey
	// Uniques are the table's UNIQUE constraints — invisible before pass #49
	// B49-10, because the index read skips constraint-backed indexes and the
	// constraint read took only FKs and checks. UniquesRead says they were
	// READ: absent is not empty, and a reader that did not look must not be
	// taken for a table that has none.
	Uniques     []ObservedUnique
	UniquesRead bool
	// Checks are the table's CHECK constraints, by name.
	Checks []string
	// CheckMembers is the MEMBER SET of every membership check, by constraint
	// name — the numbers or strings a `CHECK col IN (…)` admits.
	//
	// Names alone cannot see this one change. A membership check derived from
	// a growing catalogue (`auto_choices: ACL_PERMISSION_IDS`) keeps its name
	// for life and rewrites its BODY every time the catalogue gains a member,
	// so a sync comparing names reports "unchanged" for a database whose
	// CHECK still admits the set it was created with. Everything added since
	// is then refused at INSERT, by a constraint the schema step just said
	// was current.
	//
	// Only membership checks are captured, not check bodies in general: the
	// member set survives Postgres rewriting the expression, which is exactly
	// why comparing whole expressions was rejected.
	CheckMembers map[string][]string
	// CheckDefs is every CHECK constraint's full definition, by name, exactly
	// as `pg_get_constraintdef` renders it — the raw text; this reader
	// interprets nothing. The console uses it to drop an undeclared
	// constraint under its real identity with a DOWN that re-creates it, and
	// (pass #49 M4) to read a declared check's bounds / pattern back as facts
	// and to compare a raw check's normalised body.
	CheckDefs map[string]string
}

// ObservedForeignKey is one FK constraint: the columns it constrains and
// what it points at.
type ObservedForeignKey struct {
	Name string
	// Columns are every column the key constrains, in the database's order.
	// Plural because a scope-preserving key constrains two, and the pair —
	// not either one alone — is what identifies it against a declaration.
	Columns     []string
	TargetTable string
	// TargetSchema is the namespace the referenced table lives in. A
	// table's identity is the pair: without it a key into `other.u` is
	// indistinguishable from one into `public.u` (pass #49 B49-12 / B49-15).
	// Empty means not reported (an older reader).
	TargetSchema string
	// TargetColumn is the FIRST referenced column — kept for consumers that
	// predate TargetColumns.
	TargetColumn string
	// TargetColumns are every referenced column, paired position by
	// position with Columns (`confkey` order). Empty means not reported.
	TargetColumns []string
	// OnDelete is the key's deletion rule in its SQL spelling — "NO ACTION",
	// "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT". Carried because a
	// rule flip is otherwise invisible end to end: a stale CASCADE keeps
	// deleting data through a relationship the author turned off. Empty
	// means the rule was NOT read (an older reader), which a consumer must
	// treat as "do not compare", never as "NO ACTION".
	OnDelete string
	// DeleteSetColumns are the columns the SET NULL / SET DEFAULT action is
	// limited to (`confdelsetcols`, PostgreSQL 15+: `SET NULL (col)`); empty
	// means every referencing column. The two forms report the same
	// OnDelete, and on a scope-preserving key the bare one nulls the tenant
	// column, so a parent with children cannot be deleted.
	DeleteSetColumns []string
	// DeleteSetColumnsRead says DeleteSetColumns was read. False from an
	// older reader or a database before PostgreSQL 15: do not compare.
	DeleteSetColumnsRead bool
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
		idx, pk, pkName, err := pgObserveIndexes(ctx, conn, r.schema, r.name)
		if err != nil {
			return Observed{}, err
		}
		fks, checks, members, defs, err := pgObserveConstraints(ctx, conn, r.schema, r.name)
		if err != nil {
			return Observed{}, err
		}
		uniques, err := pgObserveUniques(ctx, conn, r.schema, r.name)
		if err != nil {
			return Observed{}, err
		}
		out.Tables = append(out.Tables, ObservedTable{
			Schema: r.schema, Name: r.name, Columns: cols, Indexes: idx, PrimaryKey: pk, PrimaryKeyName: pkName,
			ForeignKeys: fks, Checks: checks, CheckMembers: members, CheckDefs: defs,
			Uniques: uniques, UniquesRead: true,
		})
	}
	enums, err := pgObserveEnumTypes(ctx, conn)
	if err != nil {
		return Observed{}, err
	}
	out.EnumTypes = enums
	return out, nil
}

// pgObserveEnumTypes reads every native enum type with its labels.
//
// The same namespace rules as the table read: the server's own schemas are
// out, and so is anything an EXTENSION owns — the type itself, or one living
// in a schema the extension created. A type no migration made is not this
// reader's to report.
//
// Labels come in `enumsortorder`, not alphabetically: the order is part of
// the type (it is what `<` and ORDER BY use), so a reorder is a change.
func pgObserveEnumTypes(ctx context.Context, conn PgxQuerier) ([]ObservedEnumType, error) {
	rows, err := conn.Query(ctx, `
		SELECT n.nspname,
		       t.typname,
		       format_type(t.oid, NULL),
		       COALESCE((SELECT array_agg(e.enumlabel::text ORDER BY e.enumsortorder)
		                   FROM pg_enum e WHERE e.enumtypid = t.oid), '{}')
		  FROM pg_type t
		  JOIN pg_namespace n ON n.oid = t.typnamespace
		 WHERE t.typtype = 'e'
		   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		   AND n.nspname NOT LIKE 'pg\_%'
		   AND NOT EXISTS (
		         SELECT 1 FROM pg_depend d
		          WHERE d.classid = 'pg_type'::regclass
		            AND d.objid = t.oid AND d.deptype = 'e')
		   AND NOT EXISTS (
		         SELECT 1 FROM pg_depend d
		          WHERE d.classid = 'pg_namespace'::regclass
		            AND d.objid = n.oid AND d.deptype = 'e')
		 ORDER BY n.nspname, t.typname`)
	if err != nil {
		return nil, fmt.Errorf("postgres observe enum types: %w", err)
	}
	defer rows.Close()
	var out []ObservedEnumType
	for rows.Next() {
		var et ObservedEnumType
		if err := rows.Scan(&et.Schema, &et.Name, &et.Type, &et.Labels); err != nil {
			return nil, fmt.Errorf("postgres observe scan enum type: %w", err)
		}
		out = append(out, et)
	}
	return out, rows.Err()
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
func pgObserveIndexes(ctx context.Context, conn PgxQuerier, schema, table string) ([]ObservedIndex, []string, string, error) {
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
		return nil, nil, "", fmt.Errorf("postgres observe indexes %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []ObservedIndex
	for rows.Next() {
		var name, def string
		var unique, primary bool
		var cols []string
		if err := rows.Scan(&name, &unique, &primary, &def, &cols); err != nil {
			return nil, nil, "", fmt.Errorf("postgres observe scan index %s.%s: %w", schema, table, err)
		}
		out = append(out, ObservedIndex{Name: name, Unique: unique, Definition: def})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", fmt.Errorf("postgres observe indexes %s.%s: %w", schema, table, err)
	}

	pk, pkName, err := pgObservePrimaryKey(ctx, conn, schema, table)
	if err != nil {
		return nil, nil, "", err
	}
	return out, pk, pkName, nil
}

// pgObservePrimaryKey reads the primary key's columns, in key order, and
// the name of the CONSTRAINT that owns the key's index (pass #49 B49-6: the
// name is not derivable — see ObservedTable.PrimaryKeyName).
func pgObservePrimaryKey(ctx context.Context, conn PgxQuerier, schema, table string) ([]string, string, error) {
	rows, err := conn.Query(ctx, `
		SELECT a.attname,
		       COALESCE((SELECT c.conname FROM pg_constraint c
		                  WHERE c.conindid = i.indexrelid AND c.conrelid = i.indrelid
		                    AND c.contype = 'p'), '')
		  FROM pg_index i
		  JOIN pg_class tc ON tc.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = tc.relnamespace
		  JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		 WHERE n.nspname = $1 AND tc.relname = $2 AND i.indisprimary
		 ORDER BY k.ord`,
		schema, table)
	if err != nil {
		return nil, "", fmt.Errorf("postgres observe primary key %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []string
	var keyName string
	for rows.Next() {
		var name, conname string
		if err := rows.Scan(&name, &conname); err != nil {
			return nil, "", fmt.Errorf("postgres observe scan primary key %s.%s: %w", schema, table, err)
		}
		out = append(out, name)
		keyName = conname
	}
	return out, keyName, rows.Err()
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
	// attgenerated / attidentity travel too (pass #48 F35): a GENERATED
	// column stores its generation expression in pg_attrdef — the same slot
	// a DEFAULT lives in — and an IDENTITY column has no pg_attrdef row at
	// all. Without the two flags the one reads as a phantom default and the
	// other as "no default", and the reconstruction plans against a lie.
	rows, err := conn.Query(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
		       a.attgenerated <> '',
		       a.attidentity <> ''
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
		var nullable, generated, identity bool
		if err := rows.Scan(&name, &dtype, &nullable, &def, &generated, &identity); err != nil {
			return nil, fmt.Errorf("postgres observe scan column %s.%s: %w", schema, table, err)
		}
		out = append(out, Column{Name: name, DataType: dtype, Nullable: nullable, Default: def,
			Generated: generated, Identity: identity})
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
//
// INHERITED constraints are not: a constraint a partition or an INHERITS
// child receives from its parent (`conparentid` set, or `conislocal` false)
// cannot be dropped on the child, so reported it would read as a leftover
// and be planned for a DROP that Postgres refuses. It is the parent's row —
// which IS local there — that represents it.
func pgObserveConstraints(ctx context.Context, conn PgxQuerier, schema, table string) ([]ObservedForeignKey, []string, map[string][]string, map[string]string, error) {
	rows, err := conn.Query(ctx, `
		SELECT c.conname,
		       c.contype,
		       COALESCE(pg_get_constraintdef(c.oid), ''),
		       COALESCE((SELECT array_agg(a.attname ORDER BY k.ord)
		                   FROM unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
		                   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum), '{}'),
		       COALESCE(ft.relname, ''),
		       COALESCE(fn.nspname, ''),
		       COALESCE((SELECT array_agg(a.attname ORDER BY k.ord)
		                   FROM unnest(c.confkey) WITH ORDINALITY AS k(attnum, ord)
		                   JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.attnum), '{}'),
		       c.confdeltype,
		       -- confdelsetcols exists from PostgreSQL 15; read through
		       -- to_jsonb so the query also runs on an older server, where
		       -- the key is absent and the list is reported as not read.
		       (to_jsonb(c) ? 'confdelsetcols'),
		       COALESCE((SELECT array_agg(a.attname ORDER BY k.ord)
		                   FROM jsonb_array_elements_text(COALESCE(NULLIF(to_jsonb(c)->'confdelsetcols', 'null'::jsonb), '[]'::jsonb)) WITH ORDINALITY AS k(attnum, ord)
		                   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum::int2), '{}')
		  FROM pg_constraint c
		  JOIN pg_class tc ON tc.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = tc.relnamespace
		  LEFT JOIN pg_class ft ON ft.oid = c.confrelid
		  LEFT JOIN pg_namespace fn ON fn.oid = ft.relnamespace
		 WHERE n.nspname = $1 AND tc.relname = $2 AND c.contype IN ('f', 'c')
		   AND c.conislocal AND c.conparentid = 0
		 ORDER BY c.conname`,
		schema, table)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("postgres observe constraints %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var fks []ObservedForeignKey
	var checks []string
	members := map[string][]string{}
	defs := map[string]string{}
	for rows.Next() {
		var name, def, target, targetSchema string
		var cols, targetCols []string
		var kind, delType byte
		var delSetRead bool
		var delSetCols []string
		if err := rows.Scan(&name, &kind, &def, &cols, &target, &targetSchema, &targetCols, &delType, &delSetRead, &delSetCols); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("postgres observe scan constraint %s.%s: %w", schema, table, err)
		}
		switch kind {
		case 'f':
			fk := ObservedForeignKey{
				Name: name, Columns: cols, TargetTable: target, TargetSchema: targetSchema,
				TargetColumns: targetCols, OnDelete: fkRuleSQL(delType),
				DeleteSetColumns: delSetCols, DeleteSetColumnsRead: delSetRead,
			}
			if len(targetCols) > 0 {
				fk.TargetColumn = targetCols[0]
			}
			fks = append(fks, fk)
		case 'c':
			checks = append(checks, name)
			defs[name] = def
			if m, ok := CheckMembersFromDef(def); ok {
				members[name] = m
			}
		}
	}
	return fks, checks, members, defs, rows.Err()
}

// ObservedUnique is one UNIQUE constraint: its name and its columns in key
// order.
type ObservedUnique struct {
	Name    string
	Columns []string
}

// pgObserveUniques reads a table's UNIQUE constraints (contype 'u'), with the
// same local-only rule the other constraints follow (an inherited clone
// cannot be dropped on the child).
func pgObserveUniques(ctx context.Context, conn PgxQuerier, schema, table string) ([]ObservedUnique, error) {
	rows, err := conn.Query(ctx, `
		SELECT c.conname,
		       COALESCE((SELECT array_agg(a.attname ORDER BY k.ord)
		                   FROM unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
		                   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum), '{}')
		  FROM pg_constraint c
		  JOIN pg_class tc ON tc.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = tc.relnamespace
		 WHERE n.nspname = $1 AND tc.relname = $2 AND c.contype = 'u'
		   AND c.conislocal AND c.conparentid = 0
		 ORDER BY c.conname`,
		schema, table)
	if err != nil {
		return nil, fmt.Errorf("postgres observe unique constraints %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []ObservedUnique
	for rows.Next() {
		var u ObservedUnique
		if err := rows.Scan(&u.Name, &u.Columns); err != nil {
			return nil, fmt.Errorf("postgres observe scan unique constraint %s.%s: %w", schema, table, err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// fkRuleSQL spells pg_constraint.confdeltype the way SQL does. An unknown
// code answers "" — "could not read", which consumers must not compare.
func fkRuleSQL(code byte) string {
	switch code {
	case 'a':
		return "NO ACTION"
	case 'r':
		return "RESTRICT"
	case 'c':
		return "CASCADE"
	case 'n':
		return "SET NULL"
	case 'd':
		return "SET DEFAULT"
	}
	return ""
}

// CheckMembersFromDef extracts the member set of a MEMBERSHIP check from the
// definition Postgres reports, and says whether it found one.
//
// All three spellings are handled because Postgres does not preserve the one
// it was given: `CHECK (c IN (1,2))` comes back as `CHECK ((c = ANY (ARRAY[1,
// 2])))`, an unrewritten `IN (…)` survives on other engines and for some
// shapes, and a ONE-member set is rendered as a bare equality —
// `CHECK ((status = 'draft'::text))`, `CHECK ((n = 1))` — with no ARRAY and
// no IN at all (measured on postgres:16; a parser without that arm reads a
// one-member catalogue as "not a membership check" and the drift stays
// invisible). Members are returned as STRINGS so one helper serves the
// numeric and the string carrier; the caller compares them as a set, never by
// the text of the expression.
//
// Every scan is quote-aware: a comma, bracket or paren INSIDE a quoted
// member ('a,b') is part of the member, not syntax — a blind split invented
// phantom members and a spurious `choices_values_remove` on a converged
// database.
//
// Returns false for any check that is not a membership test — a length bound,
// a range, a NOT NULL emulation — because those genuinely cannot be compared
// without comparing expressions, which is what this file refuses to do.
func CheckMembersFromDef(def string) ([]string, bool) {
	if open := indexOutsideQuotes(def, "ARRAY["); open >= 0 {
		rest := def[open+len("ARRAY["):]
		end := indexOutsideQuotes(rest, "]")
		if end < 0 {
			return nil, false
		}
		return splitCheckMembers(rest[:end]), true
	}
	// `IN (…)` survives when the expression was not rewritten (other engines,
	// and Postgres for some shapes). Uppercased copy for the match only —
	// quoting positions are byte-identical.
	if in := indexOutsideQuotes(strings.ToUpper(def), " IN ("); in >= 0 {
		rest := def[in+len(" IN ("):]
		// The first UNQUOTED close paren: the member list ends there, and
		// the trailing ones belong to the CHECK wrapper Postgres adds.
		end := indexOutsideQuotes(rest, ")")
		if end < 0 {
			return nil, false
		}
		return splitCheckMembers(rest[:end]), true
	}
	return singleCheckMember(def)
}

// singleCheckMember reads the bare-equality form Postgres renders for a
// one-member set: `CHECK ((col = 'x'::text))` / `CHECK ((col = 1))`.
//
// Deliberately strict, because ` = ` appears in checks that are NOT
// membership tests: the LEFT side must be a bare column (an identifier and
// nothing else — `length(title) = 5` is not a membership check), the RIGHT
// side must be a single literal (a quoted string or a number — `price =
// round(price)` and `a = b` are not), and after the literal and the cast
// Postgres appends only the CHECK wrapper's closing parens may remain (an OR
// of equalities is not).
func singleCheckMember(def string) ([]string, bool) {
	eq := indexOutsideQuotes(def, " = ")
	if eq < 0 {
		return nil, false
	}
	lhs := strings.TrimPrefix(def[:eq], "CHECK")
	lhs = strings.TrimLeft(lhs, " (")
	// A VARCHAR (or CHAR) column comes back CAST on the left —
	// `(((v1)::text = 'draft'::text))`, measured on postgres:14/16/18 — so
	// the column is the parenthesised operand of a cast, not a bare
	// identifier (pass #49 B49-8). Unwrapped only when what follows the
	// paren is a cast and nothing else; whether what is INSIDE is a bare
	// column is still decided below, so `(lower(v))::text` stays out.
	if i := strings.Index(lhs, ")::"); i >= 0 && trimTrailingCast(lhs[i+1:]) == "" {
		lhs = lhs[:i]
	}
	lhs = strings.TrimSuffix(strings.TrimPrefix(lhs, `"`), `"`)
	if !isBareIdentifier(lhs) {
		return nil, false
	}
	member, rest, ok := literalAndTail(def[eq+len(" = "):])
	if !ok {
		return nil, false
	}
	rest = trimTrailingCast(rest)
	if strings.Trim(rest, ") ") != "" {
		return nil, false
	}
	return []string{member}, true
}

// literalAndTail consumes one leading SQL literal — a single-quoted string
// (” escaping a quote) or a bare number — returning its value and what
// follows it.
func literalAndTail(s string) (string, string, bool) {
	if strings.HasPrefix(s, "'") {
		var val strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '\'' {
				val.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '\'' {
				val.WriteByte('\'')
				i++
				continue
			}
			return val.String(), s[i+1:], true
		}
		return "", "", false // unterminated
	}
	end := 0
	for end < len(s) && (s[end] == '-' || s[end] == '+' || s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	if end == 0 {
		return "", "", false
	}
	return s[:end], s[end:], true
}

// trimTrailingCast drops the `::type` Postgres appends to a literal —
// including a parameterised one (`::character varying(3)`) — from the front
// of the tail.
func trimTrailingCast(rest string) string {
	if !strings.HasPrefix(rest, "::") {
		return rest
	}
	i := 2
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '_' ||
		(rest[i] >= 'a' && rest[i] <= 'z') || (rest[i] >= 'A' && rest[i] <= 'Z') ||
		(rest[i] >= '0' && rest[i] <= '9')) {
		i++
	}
	if i < len(rest) && rest[i] == '(' {
		if close := strings.IndexByte(rest[i:], ')'); close >= 0 {
			i += close + 1
		}
	}
	return rest[i:]
}

// isBareIdentifier reports whether s is a plain SQL identifier — the only
// left-hand side a membership equality can have.
func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == '$':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// indexOutsideQuotes is strings.Index restricted to positions outside
// single-quoted SQL literals (” escapes a quote inside one).
func indexOutsideQuotes(s, sub string) int {
	inQuote := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			if inQuote && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inQuote = !inQuote
			continue
		}
		if !inQuote && strings.HasPrefix(s[i:], sub) {
			return i
		}
	}
	return -1
}

// splitCheckMembers turns a comma-separated member list into normalised
// members: quotes and per-member casts dropped, whitespace trimmed. A cast is
// stripped because Postgres adds one (`'draft'::text`) that the declaration
// never wrote — and it is stripped AFTER the quoted value is read, so a
// member whose value itself contains `::` (or a comma, or a bracket) comes
// through intact.
func splitCheckMembers(list string) []string {
	var out []string
	for _, raw := range splitOutsideQuotes(list) {
		m := strings.TrimSpace(raw)
		if strings.HasPrefix(m, "'") {
			if val, _, ok := literalAndTail(m); ok {
				if val != "" {
					out = append(out, val)
				}
				continue
			}
		}
		if cast := strings.Index(m, "::"); cast >= 0 {
			m = strings.TrimSpace(m[:cast])
		}
		m = strings.Trim(m, "'\"")
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

// splitOutsideQuotes splits on commas that are not inside a single-quoted
// SQL literal.
func splitOutsideQuotes(s string) []string {
	var out []string
	start := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			if inQuote && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inQuote = !inQuote
		case s[i] == ',' && !inQuote:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
