// Package dqlbind provides runtime SQL helpers for dialects
// that don't support array-typed bindings (MySQL, SQLite). The
// codegen wraps `IN (:<repeated_field>)` clauses with a
// single `?` placeholder at codegen time; at runtime the
// generated body slices the request field into N values, calls
// [ExpandIn] to rewrite `IN (?)` into `IN (?, ?, …)` with N
// placeholders, and passes the per-element args alongside.
//
// PG bundles do NOT use this — the lib/pq path takes the
// `= ANY($N) + pq.Array(slice)` shape, which uses the array
// binding directly. The dispatch happens in
// the storage codegen based on the
// per-method dialect.
package dqlbind

import (
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// ExpandIn rewrites the `occurrence`-th `?` placeholder in
// `sql` (1-indexed, counting from the start of the string)
// into `(?, ?, …)` with `n` placeholders. The expansion
// REPLACES the original `?` directly — callers must wrap
// the placeholder in parens themselves (the DQL `IN (...)`
// shape always emits `IN (?)`, so the surrounding parens
// stay intact and the result reads `IN (?, ?, ?)`).
//
// `n=0` rewrites to `NULL` instead of `()` because empty
// `IN ()` is a SQL syntax error in MySQL and a conformant
// SQLite empty list produces `unrecognized token`. `IN
// (NULL)` matches no rows, which is the intended semantic
// for an empty filter list — the caller should still pass
// zero args.
//
// Behaviour:
//
//   - n > 0 and the n-th `?` is found → replace with
//     `?, ?, ?` (n times, comma-separated, no surrounding
//     parens).
//   - n == 0 → replace with `NULL` (empty-list semantic).
//   - the n-th `?` doesn't exist → return sql unchanged
//     (caller passed the wrong index; deferred to the
//     SQL driver to surface a "wrong number of args"
//     error rather than silent corruption).
//
// `occurrence` is 1-indexed to match the codegen convention
// (`arrayBindIdx` is the 1-indexed position in the binding
// list AND the 1-indexed `?` occurrence in the emitted SQL —
// MySQL/SQLite walkers emit one `?` per binding).
//
// String literals containing `?` are NOT specially handled.
// DQL strings come from proto files and the walker emits
// SQL with bindings in lockstep with `?` count, so a literal
// `?` inside a string would always shift the `arrayBindIdx`
// off-by-one — the generator catches that mismatch upstream
// (no current DQL surface produces a literal `?`).
func ExpandIn(sql string, occurrence int, n int) string {
	if occurrence < 1 {
		return sql
	}
	idx := nthQuestionMark(sql, occurrence)
	if idx < 0 {
		return sql
	}
	if n == 0 {
		return sql[:idx] + "NULL" + sql[idx+1:]
	}
	if n == 1 {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql) + (n-1)*3)
	b.WriteString(sql[:idx])
	b.WriteString("?")
	for i := 1; i < n; i++ {
		b.WriteString(", ?")
	}
	b.WriteString(sql[idx+1:])
	return b.String()
}

// ExpandValuesPG clones the single `VALUES ( ... )` tuple in
// `sql` into `n` cloned tuples with renumbered PG placeholders.
// Used by the multi-row INSERT codegen path on PG:
//
//	INSERT INTO t (a, b, c) VALUES ($1, $2, $3) RETURNING id
//
// for n=3 becomes
//
//	INSERT INTO t (a, b, c)
//	VALUES ($1, $2, $3), ($4, $5, $6), ($7, $8, $9)
//	RETURNING id
//
// `tupleSize` is the placeholder count inside the original
// tuple (codegen knows this from the bindings list and passes
// it in — the helper trusts the caller). All placeholders in
// `sql` must sit inside the VALUES tuple; codegen guarantees
// that for multi-row INSERT (RETURNING uses column refs, not
// param placeholders).
//
// Behaviour:
//
//   - n <= 1 returns sql unchanged. Codegen short-circuits
//     n == 0 (no-row INSERT is a no-op; the caller returns an
//     empty response before invoking the helper). n == 1
//     reduces to the regular single-row INSERT shape.
//   - No `VALUES (` found → returns sql unchanged. Should not
//     happen in practice (codegen-emitted SQL always has the
//     clause); the silent passthrough keeps the helper
//     defensive without raising at runtime.
//
// PG-only: see [ExpandValuesQM] for the MySQL / SQLite
// `?`-placeholder variant.
func ExpandValuesPG(sql string, tupleSize, n int) string {
	if n <= 1 || tupleSize <= 0 {
		return sql
	}
	tupleStart, tupleEnd, ok := findValuesTuple(sql)
	if !ok {
		return sql
	}
	tuple := sql[tupleStart : tupleEnd+1]
	var b strings.Builder
	b.Grow(len(sql) + (n-1)*len(tuple))
	b.WriteString(sql[:tupleStart])
	b.WriteString(tuple)
	for i := 1; i < n; i++ {
		b.WriteString(", ")
		b.WriteString(renumberPG(tuple, i*tupleSize))
	}
	b.WriteString(sql[tupleEnd+1:])
	return b.String()
}

// ExpandValuesQM is [ExpandValuesPG]'s MySQL / SQLite
// counterpart — clones the single VALUES tuple `n` times. No
// renumbering needed because `?` placeholders are positional
// (the binding order is the iteration order, which codegen
// maintains in the args slice).
//
//	INSERT INTO t (a, b, c) VALUES (?, ?, ?) RETURNING id
//
// for n=3 becomes
//
//	INSERT INTO t (a, b, c)
//	VALUES (?, ?, ?), (?, ?, ?), (?, ?, ?)
//	RETURNING id
//
// `tupleSize` is accepted for symmetry with [ExpandValuesPG]
// (and as a sanity guard — when zero / negative the helper
// returns sql unchanged, matching the PG behaviour).
//
// Behaviour:
//   - n <= 1 or tupleSize <= 0 → sql unchanged.
//   - No `VALUES (` found → sql unchanged.
func ExpandValuesQM(sql string, tupleSize, n int) string {
	if n <= 1 || tupleSize <= 0 {
		return sql
	}
	tupleStart, tupleEnd, ok := findValuesTuple(sql)
	if !ok {
		return sql
	}
	tuple := sql[tupleStart : tupleEnd+1]
	var b strings.Builder
	b.Grow(len(sql) + (n-1)*len(tuple))
	b.WriteString(sql[:tupleStart])
	b.WriteString(tuple)
	for i := 1; i < n; i++ {
		b.WriteString(", ")
		b.WriteString(tuple)
	}
	b.WriteString(sql[tupleEnd+1:])
	return b.String()
}

// findValuesTuple locates the open paren of the first `VALUES
// (` clause and its matching close paren, returning the byte
// indices of `(` and `)` inclusive. The detector walks paren
// depth so subqueries inside the tuple don't trip the
// matcher. Returns ok=false when no VALUES tuple is found.
func findValuesTuple(sql string) (start, end int, ok bool) {
	idx := strings.Index(sql, "VALUES (")
	if idx < 0 {
		return 0, 0, false
	}
	start = idx + len("VALUES ")
	depth := 0
	inStr := false // inside a single-quoted SQL string literal
	for i := start; i < len(sql); i++ {
		c := sql[i]
		if inStr {
			// Q47-dql-1: skip parens inside a string literal so a value
			// like 'a)b' isn't mistaken for the tuple's closing paren.
			// A doubled quote ('') is an escaped quote — stay in the string.
			if c == '\'' {
				if i+1 < len(sql) && sql[i+1] == '\'' {
					i++
					continue
				}
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return start, i, true
			}
		}
	}
	return 0, 0, false
}

// renumberPG returns a copy of `tuple` with every `$<digits>`
// placeholder shifted by `offset`. Non-placeholder content
// (commas, identifiers, function calls) passes through
// verbatim.
func renumberPG(tuple string, offset int) string {
	var b strings.Builder
	b.Grow(len(tuple))
	for i := 0; i < len(tuple); {
		if tuple[i] == '$' && i+1 < len(tuple) && tuple[i+1] >= '0' && tuple[i+1] <= '9' {
			j := i + 1
			for j < len(tuple) && tuple[j] >= '0' && tuple[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(tuple[i+1 : j])
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n + offset))
			i = j
			continue
		}
		b.WriteByte(tuple[i])
		i++
	}
	return b.String()
}

// nthQuestionMark returns the byte index of the `n`-th `?`
// in `s` (1-indexed), or -1 if there are fewer than `n`
// occurrences. SQL identifiers are not parsed — callers
// passing strings that contain literal `?` characters will
// get incorrect results (see [ExpandIn] doc).
func nthQuestionMark(s string, n int) int {
	if n < 1 {
		return -1
	}
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '?' {
			count++
			if count == n {
				return i
			}
		}
	}
	return -1
}

// NullIfEmpty returns `nil` when `v` carries a zero-length
// payload, otherwise returns `v` unchanged. The codegen wraps
// SQL bindings for `(w17.field).null = true` scalar fields
// with this helper so proto3 zero values route to SQL NULL:
//   - empty string `""` on a nullable UUID column would land
//     as `invalid input syntax for type uuid: ""` on PG
//   - empty `[]byte{}` on a nullable JSONB / bytea column
//     would land as a zero-length blob rather than NULL,
//     breaking the conventional "field unset = NULL" semantic
//
// Generic `any` parameter keeps one binding emit shape across
// kinds; the type switch routes per runtime type. Untyped nil
// is what lib/pq translates to SQL NULL — typed nil pointers
// go through reflect and break the cast on UUID columns.
func NullIfEmpty(v any) any {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
	case []byte:
		if len(x) == 0 {
			return nil
		}
	}
	return v
}

// TimestampOrNull routes a google.protobuf.Timestamp param binding to
// SQL NULL when the message is unset (nil), instead of the year-1 zero
// time that `(*timestamppb.Timestamp)(nil).AsTime()` returns. An unset
// proto field means "no value" → NULL, never 0001-01-01: binding the
// zero time would, on a nullable column, store a bogus far-past value
// (e.g. an `expires_at` that the validation guard then reads as already
// expired) and, on a NOT NULL column, mask a missing-required-value bug
// behind a silently-stored sentinel. A set Timestamp binds as its
// time.Time, which lib/pq sends as TIMESTAMPTZ — same as the bare
// `.AsTime()` the codegen used before.
func TimestampOrNull(ts *timestamppb.Timestamp) any {
	if ts == nil {
		return nil
	}
	return ts.AsTime()
}
