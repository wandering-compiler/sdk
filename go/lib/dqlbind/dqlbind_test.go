package dqlbind

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestExpandIn(t *testing.T) {
	cases := []struct {
		name       string
		sql        string
		occurrence int
		n          int
		want       string
	}{
		{
			name:       "single bind, expand to 3",
			sql:        "SELECT * FROM users WHERE id IN (?)",
			occurrence: 1,
			n:          3,
			want:       "SELECT * FROM users WHERE id IN (?, ?, ?)",
		},
		{
			name:       "single bind, expand to 1 = no-op",
			sql:        "SELECT * FROM users WHERE id IN (?)",
			occurrence: 1,
			n:          1,
			want:       "SELECT * FROM users WHERE id IN (?)",
		},
		{
			name:       "single bind, n=0 → NULL",
			sql:        "SELECT * FROM users WHERE id IN (?)",
			occurrence: 1,
			n:          0,
			want:       "SELECT * FROM users WHERE id IN (NULL)",
		},
		{
			name:       "second occurrence among multiple ? — only that one expands",
			sql:        "SELECT * FROM users WHERE country = ? AND id IN (?) AND age > ?",
			occurrence: 2,
			n:          3,
			want:       "SELECT * FROM users WHERE country = ? AND id IN (?, ?, ?) AND age > ?",
		},
		{
			name:       "third occurrence — last one expands",
			sql:        "SELECT * FROM t WHERE a = ? AND b = ? AND c IN (?)",
			occurrence: 3,
			n:          2,
			want:       "SELECT * FROM t WHERE a = ? AND b = ? AND c IN (?, ?)",
		},
		{
			name:       "occurrence out of range — sql unchanged",
			sql:        "SELECT * FROM users WHERE id IN (?)",
			occurrence: 2,
			n:          3,
			want:       "SELECT * FROM users WHERE id IN (?)",
		},
		{
			name:       "occurrence < 1 — sql unchanged",
			sql:        "SELECT * FROM users WHERE id IN (?)",
			occurrence: 0,
			n:          3,
			want:       "SELECT * FROM users WHERE id IN (?)",
		},
		{
			name:       "no question marks — unchanged",
			sql:        "SELECT 1",
			occurrence: 1,
			n:          5,
			want:       "SELECT 1",
		},
		{
			name:       "n=0 with mid-string occurrence",
			sql:        "SELECT * FROM t WHERE a = ? AND b IN (?)",
			occurrence: 2,
			n:          0,
			want:       "SELECT * FROM t WHERE a = ? AND b IN (NULL)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpandIn(tc.sql, tc.occurrence, tc.n)
			if got != tc.want {
				t.Errorf("ExpandIn(%q, %d, %d) = %q, want %q",
					tc.sql, tc.occurrence, tc.n, got, tc.want)
			}
		})
	}
}

func TestExpandValuesPG(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		tupleSize int
		n         int
		want      string
	}{
		{
			name:      "n=3 renumbers cloned tuples",
			sql:       "INSERT INTO t (a, b, c) VALUES ($1, $2, $3) RETURNING id",
			tupleSize: 3,
			n:         3,
			want:      "INSERT INTO t (a, b, c) VALUES ($1, $2, $3), ($4, $5, $6), ($7, $8, $9) RETURNING id",
		},
		{
			name:      "n=1 no-op",
			sql:       "INSERT INTO t (a) VALUES ($1)",
			tupleSize: 1,
			n:         1,
			want:      "INSERT INTO t (a) VALUES ($1)",
		},
		{
			name:      "n=0 no-op",
			sql:       "INSERT INTO t (a) VALUES ($1)",
			tupleSize: 1,
			n:         0,
			want:      "INSERT INTO t (a) VALUES ($1)",
		},
		{
			name:      "mixed shape — shared placeholder renumbers per row",
			sql:       "INSERT INTO t (q, c, inv) VALUES ($1, $2, $3) RETURNING id",
			tupleSize: 3,
			n:         2,
			want:      "INSERT INTO t (q, c, inv) VALUES ($1, $2, $3), ($4, $5, $6) RETURNING id",
		},
		{
			name:      "no VALUES — passthrough",
			sql:       "SELECT * FROM t",
			tupleSize: 1,
			n:         3,
			want:      "SELECT * FROM t",
		},
		{
			name:      "two-digit placeholders renumber correctly",
			sql:       "INSERT INTO t (a, b, c, d, e, f, g, h, i, j, k) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)",
			tupleSize: 11,
			n:         2,
			want:      "INSERT INTO t (a, b, c, d, e, f, g, h, i, j, k) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11), ($12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)",
		},
		{
			// Q47-dql-1: a string-literal column value containing a `)`
			// must not be mistaken for the tuple's closing paren.
			name:      "string literal with paren — not a tuple boundary",
			sql:       "INSERT INTO t (x, note) VALUES ($1, 'a)b') RETURNING id",
			tupleSize: 1,
			n:         2,
			want:      "INSERT INTO t (x, note) VALUES ($1, 'a)b'), ($2, 'a)b') RETURNING id",
		},
		{
			// Escaped quote ('') inside the literal keeps the scanner in
			// the string until the real closing quote.
			name:      "string literal with escaped quote and paren",
			sql:       "INSERT INTO t (x, note) VALUES ($1, 'it''s (x)') RETURNING id",
			tupleSize: 1,
			n:         2,
			want:      "INSERT INTO t (x, note) VALUES ($1, 'it''s (x)'), ($2, 'it''s (x)') RETURNING id",
		},
		{
			// VALUES ( with no matching close paren: the depth walk never
			// returns to 0 and findValuesTuple falls through to ok=false, so
			// the helper leaves the (malformed) SQL untouched rather than
			// corrupting it. Codegen never emits this; the guard is defensive.
			name:      "unterminated tuple — passthrough",
			sql:       "INSERT INTO t (a) VALUES ($1",
			tupleSize: 1,
			n:         2,
			want:      "INSERT INTO t (a) VALUES ($1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpandValuesPG(tc.sql, tc.tupleSize, tc.n)
			if got != tc.want {
				t.Errorf("ExpandValuesPG(%q, %d, %d):\n got: %q\nwant: %q", tc.sql, tc.tupleSize, tc.n, got, tc.want)
			}
		})
	}
}

func TestExpandValuesQM(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		tupleSize int
		n         int
		want      string
	}{
		{
			name:      "n=3 clones tuples",
			sql:       "INSERT INTO t (a, b, c) VALUES (?, ?, ?) RETURNING id",
			tupleSize: 3,
			n:         3,
			want:      "INSERT INTO t (a, b, c) VALUES (?, ?, ?), (?, ?, ?), (?, ?, ?) RETURNING id",
		},
		{
			name:      "n=1 no-op",
			sql:       "INSERT INTO t (a) VALUES (?)",
			tupleSize: 1,
			n:         1,
			want:      "INSERT INTO t (a) VALUES (?)",
		},
		{
			name:      "no VALUES — passthrough",
			sql:       "UPDATE t SET a = ? WHERE id = ?",
			tupleSize: 1,
			n:         3,
			want:      "UPDATE t SET a = ? WHERE id = ?",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpandValuesQM(tc.sql, tc.tupleSize, tc.n)
			if got != tc.want {
				t.Errorf("ExpandValuesQM(%q, %d, %d):\n got: %q\nwant: %q", tc.sql, tc.tupleSize, tc.n, got, tc.want)
			}
		})
	}
}

// TestNullIfEmpty pins the proto3-zero → SQL NULL routing for nullable
// scalar bindings: only an empty string or zero-length []byte degrade to
// untyped nil (what lib/pq translates to SQL NULL); every other value —
// including a non-empty string/[]byte and any non-string/[]byte type —
// passes through verbatim so the driver binds it unchanged. Untyped nil
// also passes through (the type switch matches neither arm), which is the
// correct NULL representation already.
func TestNullIfEmpty(t *testing.T) {
	cases := []struct {
		name   string
		in     any
		isNil  bool // want untyped nil
		wantEq any  // when !isNil, the value must compare equal
	}{
		{name: "empty string → nil", in: "", isNil: true},
		{name: "non-empty string passes through", in: "abc", wantEq: "abc"},
		{name: "empty bytes → nil", in: []byte{}, isNil: true},
		{name: "nil bytes → nil (len 0)", in: []byte(nil), isNil: true},
		{name: "non-empty bytes pass through", in: []byte{0x01}, wantEq: "\x01"},
		{name: "int passes through unchanged", in: 42, wantEq: 42},
		{name: "bool passes through unchanged", in: false, wantEq: false},
		{name: "untyped nil passes through", in: nil, wantEq: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NullIfEmpty(tc.in)
			if tc.isNil {
				if got != nil {
					t.Errorf("NullIfEmpty(%#v) = %#v, want nil", tc.in, got)
				}
				return
			}
			// Compare via fmt to keep []byte/string equality simple.
			if b, ok := got.([]byte); ok {
				if string(b) != tc.wantEq {
					t.Errorf("NullIfEmpty(%#v) = %q, want %q", tc.in, b, tc.wantEq)
				}
				return
			}
			if got != tc.wantEq {
				t.Errorf("NullIfEmpty(%#v) = %#v, want %#v", tc.in, got, tc.wantEq)
			}
		})
	}
}

// TestTimestampOrNull pins the unset-Timestamp → SQL NULL routing: a nil
// proto message must yield untyped nil (NULL), never the year-1 zero time
// that (*timestamppb.Timestamp)(nil).AsTime() returns; a set Timestamp must
// yield its time.Time so lib/pq sends TIMESTAMPTZ.
func TestTimestampOrNull(t *testing.T) {
	t.Run("nil → nil", func(t *testing.T) {
		if got := TimestampOrNull(nil); got != nil {
			t.Errorf("TimestampOrNull(nil) = %#v, want nil", got)
		}
	})
	t.Run("set → time.Time", func(t *testing.T) {
		want := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
		got := TimestampOrNull(timestamppb.New(want))
		gt, ok := got.(time.Time)
		if !ok {
			t.Fatalf("TimestampOrNull(set) = %T, want time.Time", got)
		}
		if !gt.Equal(want) {
			t.Errorf("TimestampOrNull(set) = %v, want %v", gt, want)
		}
	})
}

// TestNthQuestionMark covers the n<1 guard of nthQuestionMark directly:
// ExpandIn rejects occurrence<1 before ever calling it, so the guard is
// only reachable by a direct call. The found/not-found arms are exercised
// transitively by TestExpandIn but pinned here for the boundary.
func TestNthQuestionMark(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want int
	}{
		{name: "n<1 guard → -1", s: "a?b?", n: 0, want: -1},
		{name: "negative n → -1", s: "a?b?", n: -3, want: -1},
		{name: "first occurrence", s: "a?b?c", n: 1, want: 1},
		{name: "second occurrence", s: "a?b?c", n: 2, want: 3},
		{name: "out of range → -1", s: "a?b", n: 5, want: -1},
		{name: "no question marks → -1", s: "abc", n: 1, want: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nthQuestionMark(tc.s, tc.n); got != tc.want {
				t.Errorf("nthQuestionMark(%q, %d) = %d, want %d", tc.s, tc.n, got, tc.want)
			}
		})
	}
}
