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

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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
// list AND the 1-indexed `?` PLACEHOLDER in the emitted SQL —
// MySQL/SQLite walkers emit one `?` per binding).
//
// A `?` inside a single-quoted string literal is NOT a
// placeholder and is not counted (see [scanSQL]). The doc here
// used to say the opposite — "no current DQL surface produces a
// literal `?`", so counting `?` bytes was safe and "the
// generator catches that mismatch upstream". Both halves were
// false: walker.emitLiteral inlines DQL string literals
// verbatim, the DQL lexer admits `?` inside them, no generator
// check counts placeholders against bindings, and T1-4 pass #11
// live-proved the consequence — `WHERE note = 'why?' AND b IN
// (:l1) AND c IN (:l2)` expanded the two lists onto each other's
// placeholders with a matching arg count, and real SQLite
// returned the wrong row with no error.
func ExpandIn(sql string, occurrence int, n int) string {
	if occurrence < 1 {
		return sql
	}
	idx := nthPlaceholder(sql, occurrence)
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

// scanSQL walks `sql` byte by byte, in order, calling `fn` with each
// byte's index, its value, and whether it is CODE — i.e. sits OUTSIDE a
// single-quoted SQL string literal. Returning false from `fn` stops the
// walk. The delimiting quotes themselves report code=false: they belong
// to the literal, and no scanner in this package looks for them.
//
// THE RULE OF THIS FILE. Every byte-level scan here goes through this
// function. Three scanners in this package answer some form of "where is
// the n-th X in the emitted SQL", and each one used to re-derive the walk
// for itself; only findValuesTuple ever learned that a string literal is
// not code (Q47-dql-1). The other two shipped blind, and T1-4 pass #11
// found what that costs once the walker started INLINING DQL string
// literals verbatim into the emitted SQL (walker.emitLiteral,
// LITERAL_KIND_STRING):
//
//   - C-F6: a `?` inside a literal (`WHERE note = 'why?'`) shifted every
//     [ExpandIn] occurrence count on MySQL / SQLite. Live-proven: two IN
//     lists expanded onto each other's placeholders, arg count still
//     matched, and real SQLite returned a DIFFERENT ROW. No error.
//   - VC-N1: a `$<digit>` inside a literal (`note = 'fee $2 extra'`) was
//     renumbered by renumberPG into every cloned VALUES tuple, so a
//     multi-row INSERT STORED a different string in rows 2..n. No error.
//
// A fourth scanner must not be writable blind: add it here, not next to
// here. What "not code" means is deliberately narrow — single-quoted
// string literals, with SQL's doubled-quote escape — because that is
// exactly what the emitter produces: walker.emitLiteral doubles every
// embedded quote (and, on dialects that treat `\` as an escape, every
// backslash) before writing the literal, so a lone `\'` never reaches this
// scan; identifiers are proto-derived (`[a-z0-9_]`) and can carry neither
// `?` nor `$N`; and no emit path writes dollar-quoted bodies or SQL
// comments.
func scanSQL(sql string, fn func(i int, c byte, code bool) bool) {
	inStr := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case !inStr && c == '\'':
			inStr = true
		case inStr && c == '\'':
			// A doubled quote ('') is an escaped quote, not the end of
			// the literal: report both bytes and stay inside.
			if i+1 < len(sql) && sql[i+1] == '\'' {
				if !fn(i, c, false) {
					return
				}
				i++
				if !fn(i, sql[i], false) {
					return
				}
				continue
			}
			inStr = false
		}
		if !fn(i, c, !inStr && c != '\'') {
			return
		}
	}
}

// findValuesTuple locates the open paren of the first `VALUES
// (` clause and its matching close paren, returning the byte
// indices of `(` and `)` inclusive. The detector walks paren
// depth so subqueries inside the tuple don't trip the
// matcher. Returns ok=false when no VALUES tuple is found.
//
// Both halves run over [scanSQL]'s code bytes: parens inside a string
// literal are not tuple boundaries (Q47-dql-1 — `'a)b'`), and neither is
// a `VALUES (` written inside one.
func findValuesTuple(sql string) (start, end int, ok bool) {
	const marker = "VALUES ("
	depth := 0
	skipTo := -1
	scanSQL(sql, func(i int, c byte, code bool) bool {
		if !code || i < skipTo {
			return true
		}
		if depth == 0 {
			if c == 'V' && strings.HasPrefix(sql[i:], marker) {
				start = i + len(marker) - 1 // index of the `(`
				depth = 1
				skipTo = start + 1 // don't re-count that paren
			}
			return true
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
				ok = true
				return false
			}
		}
		return true
	})
	if !ok {
		return 0, 0, false
	}
	return start, end, true
}

// renumberPG returns a copy of `tuple` with every `$<digits>`
// placeholder shifted by `offset`. Non-placeholder content
// (commas, identifiers, function calls) passes through
// verbatim — INCLUDING a `$<digits>` inside a string literal,
// which is data the statement stores, not a placeholder
// (VC-N1; see [scanSQL]).
func renumberPG(tuple string, offset int) string {
	var b strings.Builder
	b.Grow(len(tuple))
	skipTo := -1
	scanSQL(tuple, func(i int, c byte, code bool) bool {
		if i < skipTo {
			return true
		}
		if code && c == '$' && i+1 < len(tuple) && isDigit(tuple[i+1]) {
			// The digit run cannot cross into a literal (a digit is not a
			// quote), so reading ahead here stays inside code.
			j := i + 1
			for j < len(tuple) && isDigit(tuple[j]) {
				j++
			}
			n, _ := strconv.Atoi(tuple[i+1 : j])
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n + offset))
			skipTo = j
			return true
		}
		b.WriteByte(c)
		return true
	})
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// nthPlaceholder returns the byte index of the `n`-th `?`
// PLACEHOLDER in `s` (1-indexed), or -1 if there are fewer than
// `n`. A `?` inside a string literal is data, not a placeholder,
// and is skipped — the driver does not bind it either, so this is
// the same counting the driver does (C-F6; see [scanSQL]).
func nthPlaceholder(s string, n int) int {
	if n < 1 {
		return -1
	}
	idx := -1
	count := 0
	scanSQL(s, func(i int, c byte, code bool) bool {
		if !code || c != '?' {
			return true
		}
		count++
		if count == n {
			idx = i
			return false
		}
		return true
	})
	return idx
}

// Bytes returns a NON-NIL byte slice, so an empty `bytes` value binds as
// the empty SQL value rather than as NULL.
//
// Arr's problem, one type over, and reachable the same way: proto3 has no
// presence for `bytes`, so an empty payload arrives as a nil slice — which
// lib/pq sends as NULL, and a NOT NULL `bytea` column refuses. The console's
// deploy registry hit it on the first EMPTY fixture pushed: a fixture with no
// rows marshals to zero bytes, the map store stores it happily, and the write
// failed with a constraint violation naming nothing the operator wrote.
//
// It composes with [NullIfEmpty] rather than fighting it: that helper nulls on
// LENGTH, so a nullable column still stores NULL for an empty value and only a
// NOT NULL one gets the empty payload.
func Bytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// Arr returns a NON-NIL slice, so an empty repeated value binds as the
// empty SQL array rather than as NULL.
//
// `pq.Array` is where the difference is decided: its Value() returns
// untyped nil for a nil slice, which lib/pq sends as NULL. A repeated
// COLUMN is declared NOT NULL — the model says "always a list, possibly
// empty" — so an empty list would fail the constraint instead of storing
// `{}`. And there is nothing the caller can do about it from the proto
// side: proto3 repeated fields have no presence, so an empty list and an
// absent one arrive identically as nil on the server.
//
// It is applied to every array binding, filters included, rather than
// only to writes. A filter binds `col = ANY($1)`, where NULL and `{}`
// both match no rows, so the coercion changes nothing there — and one
// rule ("a repeated binding is an array, never NULL") is worth more than
// a second emit path that has to know which side of the statement it is
// on.
func Arr[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
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

// EnumFieldOrNull routes a proto3-optional enum param binding to SQL NULL
// when the field is unset, and to its int32 value when set.
//
// It exists for `?` skip-on-null filters. Generated enums are
// `type Foo int32`, so the value form `int32(req.GetX())` yields 0 —
// the UNSPECIFIED sentinel — for an unset field, and `col = 0` matches
// no row while the guard's `OR $1 IS NULL` never fires. The result is
// an empty list with no error, which a caller cannot tell from
// "nothing matched".
//
// NullIfEmpty cannot do this job: its type switch covers string and
// []byte only, so an int32 zero passes through unchanged. Widening it
// to nil every numeric zero would be wrong — 0 is an ordinary value
// for an int column, and only presence says whether it was meant.
// It takes the PARENT message plus a field name rather than the `*Enum`
// pointer the generated struct exposes — `req` for a top-level param,
// `req.GetFilter()` for `:filter.status_exact`. Reading that pointer
// means emitting `req.GetFilter().StatusExact`, and a getter mid-path
// returns a NIL MESSAGE for an absent submessage, so the field read
// panics — in the exact case this helper exists to serve, "the caller
// sent nothing". It surfaced live as a 500 on an unfiltered page, which
// is indistinguishable from the empty-list defect above. An absent
// message on the path is just another way for the field to be absent,
// and going through the descriptor is what makes that true at EVERY
// depth rather than only at depth one.
//
// The codegen resolves the path against the request descriptor before
// it emits the call, so the field name is checked at build time; and it
// only emits this for a field WITH presence, since `Has` on a plain
// proto3 enum is false for the zero value and would turn UNSPECIFIED
// into NULL rather than 0.
func EnumFieldOrNull(parent proto.Message, field string) any {
	if parent == nil {
		return nil
	}
	m := parent.ProtoReflect()
	if !m.IsValid() {
		// A typed-nil message — an absent submessage on the path.
		return nil
	}
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(field))
	if fd == nil || fd.Kind() != protoreflect.EnumKind || !m.Has(fd) {
		return nil
	}
	return int32(m.Get(fd).Enum())
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

// DedupKeys collapses duplicate keys in a KV multi-fetch key list,
// preserving FIRST-occurrence order. Returns the input slice unchanged
// (no allocation) when it holds no duplicates.
//
// T2-6 pass #8 (B-F11). `WHERE pk IN (:ids)` is one declaration with two
// emitted realisations: on a SQL connection it renders `= ANY($1)`, which
// yields each matching row ONCE regardless of how many times its id appears
// in the request; on a KV connection the emitter built one key per request
// ELEMENT, so `[h, h]` fetched — and returned — the entity twice. That made
// the response multiplicity a property of the STORAGE LAYOUT rather than of
// the query, and re-homing a model from Redis onto SQL (which the compiler
// advertises as transparent) silently changed the answer.
//
// Deduplicating on the KEY rather than on the request element is deliberate:
// two distinct request ids that render the same key are the same entity, and
// the SQL side would likewise return it once.
//
// Misses stay dropped (the KV multi-fetch contract — the response carries
// only matched entities); this helper only removes REDUNDANT lookups.
func DedupKeys(keys []string) []string {
	if len(keys) < 2 {
		return keys
	}
	seen := make(map[string]struct{}, len(keys))
	dup := false
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			dup = true
			break
		}
		seen[k] = struct{}{}
	}
	if !dup {
		return keys
	}
	clear(seen)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
