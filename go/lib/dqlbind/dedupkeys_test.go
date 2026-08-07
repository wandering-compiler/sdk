package dqlbind_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/dqlbind"
)

// T2-6 pass #8, B-F11 — the KV multi-fetch emitters route their key list
// through DedupKeys so `WHERE pk IN (:ids)` answers with the same
// multiplicity on Redis as `= ANY($1)` does on SQL: each matching entity
// once, however many times the caller repeated its id.
func TestDedupKeys(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, []string{}},
		{"single", []string{"a"}, []string{"a"}},
		{"no duplicates keeps order", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"adjacent duplicate collapses", []string{"a", "a"}, []string{"a"}},
		{"non-adjacent duplicate collapses to FIRST occurrence",
			[]string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"all identical", []string{"x", "x", "x", "x"}, []string{"x"}},
		{"empty string is a key like any other", []string{"", "a", ""}, []string{"", "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dqlbind.DedupKeys(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("DedupKeys(%v) = %v; want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("DedupKeys(%v) = %v; want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// The caller's slice must not be mutated — the generated body may still
// reference the request's own repeated field elsewhere (e.g. the empty-input
// short-circuit reads its length).
func TestDedupKeys_DoesNotMutateInput(t *testing.T) {
	in := []string{"a", "b", "a"}
	_ = dqlbind.DedupKeys(in)
	want := []string{"a", "b", "a"}
	for i := range in {
		if in[i] != want[i] {
			t.Fatalf("input mutated: %v; want %v", in, want)
		}
	}
}
