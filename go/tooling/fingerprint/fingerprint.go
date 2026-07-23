// Package fingerprint computes a deterministic hash of a target
// database's schema state. The hash is the input to the Phase D
// (D-iter3-14) drift-detection check: w17migrate computes the
// current target fingerprint before applying each migration and
// compares against the migration's `expected_pre_fingerprint`
// header.
//
// **Scope.** Relational dialects only (postgres / mysql / sqlite).
// KV / QUEUE / S3 are schemaless; the bookkeeping store IS the
// schema state, and is already covered by AppliedHead. Drift
// detection there is conceptually different (data drift, not
// schema drift) and out of Phase D scope.
//
// **Algorithm.** Query information_schema (or pg_catalog /
// sqlite_master per dialect) for tables + columns; canonicalise
// to a text representation (sorted by name, normalised types);
// sha256. The text format is dialect-agnostic so callers can
// compare across compile-time and apply-time produced
// fingerprints byte-for-byte.
//
// **What's covered.** Tables (excluding the wc_migrations
// bookkeeping table itself) + their columns: name, data type,
// nullable, default. Indexes + foreign-key constraints are
// out of MVP scope — they're high-signal but require more
// canonicalisation work (pg's `indexdef` vs mysql's
// `SHOW INDEXES` differ; FK metadata lives in different
// information_schema views per dialect). Adding them later is
// a per-dialect extension to the same Format.
//
// **MVP gap.** This package is the apply-side fingerprinting
// half of Phase D. The compile-side half — generating the same
// fingerprint shape from console's IR or shadow DBs — is the
// SCAFFOLDED-but-NOT-IMPLEMENTED part: console emits
// `FAKE_<hex>` placeholders today (see decorate package), so
// the fingerprint comparison in w17migrate is intentionally
// stubbed to always-pass. Drift detection becomes operational
// when console grows shadow DB integration. See iteration-3.md
// D-iter3-14 for the swap point.
package fingerprint

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Schema is the canonical in-memory representation an extractor
// produces before hashing. Tables are sorted by name; per-table
// columns are sorted by name. Format() emits a deterministic
// text serialisation; FingerprintHex hashes it.
type Schema struct {
	Tables []Table
}

// Table is one table's contribution to the fingerprint.
type Table struct {
	Name    string
	Columns []Column
}

// Column carries the per-column attributes that go into the
// fingerprint.
type Column struct {
	Name     string
	DataType string
	Nullable bool
	Default  string // empty when no default
}

// Format produces the canonical text representation of the
// schema. Sorted, line-per-table, line-per-column.
//
//	TABLE "<table>"
//	COLUMN "<col>" "<type>" <null|notnull>[ default="<value>"]
//	COLUMN ...
//	TABLE "<next>"
//	...
//
// Every user-supplied field (table/column name, type, default) is
// %q-quoted so the serialisation is INJECTIVE: a name or default
// containing a newline / space / quote can't be rearranged to forge a
// different schema that hashes the same (the null/notnull token is a
// fixed enum, not user data, so it stays bare).
func (s Schema) Format() string {
	tables := make([]Table, len(s.Tables))
	copy(tables, s.Tables)
	sort.Slice(tables, func(i, j int) bool {
		return tables[i].Name < tables[j].Name
	})

	var b strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&b, "TABLE %q\n", t.Name)
		cols := make([]Column, len(t.Columns))
		copy(cols, t.Columns)
		sort.Slice(cols, func(i, j int) bool {
			return cols[i].Name < cols[j].Name
		})
		for _, c := range cols {
			null := "notnull"
			if c.Nullable {
				null = "null"
			}
			fmt.Fprintf(&b, "COLUMN %q %q %s", c.Name, c.DataType, null)
			if c.Default != "" {
				fmt.Fprintf(&b, " default=%q", c.Default)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// FingerprintHex returns the hex-encoded sha256 of the schema's
// Format() output. The same input produces the same hex
// regardless of dialect — fixture-matrix tests can compare
// fingerprints across PG / MySQL / SQLite extractor outputs to
// catch divergence in canonicalisation.
func (s Schema) FingerprintHex() string {
	sum := sha256.Sum256([]byte(s.Format()))
	return hex.EncodeToString(sum[:])
}

// excludedTables lists the bookkeeping tables Phase D
// fingerprinting must skip — they're an artifact of the
// migrator itself, not the user's schema, and re-applying a
// migration changes their content (the per-version row in
// wc_migrations) without representing a real schema drift.
var excludedTables = map[string]struct{}{
	"wc_migrations": {},
}

// shouldExclude reports whether `table` is on the bookkeeping
// exclusion list.
func shouldExclude(table string) bool {
	_, ok := excludedTables[table]
	return ok
}

// dbQuery is the *sql.DB.QueryContext signature in interface
// form so per-dialect extractors can be unit-tested with a
// mock.
type dbQuery interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
