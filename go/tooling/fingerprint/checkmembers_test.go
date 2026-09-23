package fingerprint

import (
	"reflect"
	"testing"
)

// Postgres does not hand back the expression it was given, which is the whole
// reason this file reads member SETS rather than comparing bodies.
func TestCheckMembersFromDef(t *testing.T) {
	cases := []struct {
		name string
		def  string
		want []string
		ok   bool
	}{{
		name: "rewritten numeric membership",
		def:  "CHECK ((permission_id = ANY (ARRAY[1, 2, 3])))",
		want: []string{"1", "2", "3"},
		ok:   true,
	}, {
		name: "rewritten string membership carries a cast per member",
		def:  "CHECK ((status = ANY (ARRAY['draft'::text, 'live'::text])))",
		want: []string{"draft", "live"},
		ok:   true,
	}, {
		name: "unrewritten IN survives",
		def:  "CHECK (permission_id IN (7, 8))",
		want: []string{"7", "8"},
		ok:   true,
	}, {
		// A length bound genuinely cannot be compared without comparing
		// expressions, so it must report "not a membership check" rather
		// than a wrong answer.
		name: "a length bound is not a membership check",
		def:  "CHECK ((length(title) <= 200))",
		ok:   false,
	}, {
		// A48-12 / F8b — Postgres renders a ONE-member set as a bare
		// equality, not as ARRAY or IN. Measured on postgres:16:
		// `CHECK (status IN ('draft'))` comes back as this.
		name: "single-member string set",
		def:  "CHECK ((status = 'draft'::text))",
		want: []string{"draft"},
		ok:   true,
	}, {
		// Same, numeric carrier: `CHECK (n IN (1))` → `CHECK ((n = 1))`.
		name: "single-member numeric set",
		def:  "CHECK ((n = 1))",
		want: []string{"1"},
		ok:   true,
	}, {
		// A quoted member may itself contain a quote, doubled by PG.
		name: "single member with an escaped quote",
		def:  "CHECK ((s = 'it''s'::text))",
		want: []string{"it's"},
		ok:   true,
	}, {
		// A48-14 / F16 — a comma INSIDE a quoted member is part of the
		// member, not a separator. Measured on postgres:16:
		// `CHECK (status IN ('a,b','c'))` renders as this; a comma-blind
		// split reports three phantom members for two real ones and a
		// spurious `choices_values_remove` on a converged database.
		name: "member containing a comma",
		def:  "CHECK ((status = ANY (ARRAY['a,b'::text, 'c'::text])))",
		want: []string{"a,b", "c"},
		ok:   true,
	}, {
		// A `]` inside a quoted member must not end the ARRAY scan.
		name: "member containing a bracket",
		def:  "CHECK ((s = ANY (ARRAY['a]b'::text, 'c'::text])))",
		want: []string{"a]b", "c"},
		ok:   true,
	}, {
		// A `)` inside a quoted member must not end the IN scan.
		name: "unrewritten IN with a paren in a member",
		def:  "CHECK (s IN ('a)b', 'c'))",
		want: []string{"a)b", "c"},
		ok:   true,
	}, {
		// A member containing `::` keeps it — only the cast PG APPENDS
		// (outside the quotes) is stripped.
		name: "member containing a double colon",
		def:  "CHECK ((s = ANY (ARRAY['a::b'::text, 'c'::text])))",
		want: []string{"a::b", "c"},
		ok:   true,
	}, {
		// The single-member arm must not swallow every equality: a range
		// bound, a column-to-expression equality and a multi-branch OR are
		// not membership tests.
		name: "a range bound is not a single-member set",
		def:  "CHECK ((n >= 1))",
		ok:   false,
	}, {
		name: "an expression equality is not a membership check",
		def:  "CHECK ((price = round(price)))",
		ok:   false,
	}, {
		name: "a function-call left side is not a membership check",
		def:  "CHECK ((length(title) = 5))",
		ok:   false,
	}, {
		name: "a column equality is not a membership check",
		def:  "CHECK ((a = b))",
		ok:   false,
	}, {
		name: "an OR of equalities is not a membership check",
		def:  "CHECK (((a = 'x'::text) OR (b = 'y'::text)))",
		ok:   false,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := CheckMembersFromDef(c.def)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (def=%q)", ok, c.ok, c.def)
			}
			if c.ok && !reflect.DeepEqual(got, c.want) {
				t.Errorf("members = %v, want %v", got, c.want)
			}
		})
	}
}
