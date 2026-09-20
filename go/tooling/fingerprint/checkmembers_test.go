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
