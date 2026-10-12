package runtime

import (
	"strings"
	"testing"
)

// A capture bound in one field of an `expect` resolves in a sibling, every
// time. Map iteration order used to decide, so the sibling saw the capture
// on some runs and failed "unresolved" on others.
func TestMatchExpect_ASiblingSeesTheCapture(t *testing.T) {
	expect := map[string]any{
		"a_echo": map[string]any{"matcher": "regex", "pattern": "^${id}$"},
		"b_same": "${id}",
		"z_id":   map[string]any{"capture": "id"},
		"nested": map[string]any{"inner": map[string]any{"capture": "inner.id"}},
		"c_uses": "${inner.id}",
	}
	actual := map[string]any{"a_echo": "x-1", "b_same": "x-1", "z_id": "x-1",
		"nested": map[string]any{"inner": "n-1"}, "c_uses": "n-1"}
	for i := 0; i < 100; i++ {
		scope := NewRun().NewScope()
		if err := MatchExpect(expect, actual, scope); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// With several fields failing, the reported one is the same on every run.
func TestMatchExpect_ReportsTheSameFailureEveryRun(t *testing.T) {
	expect := map[string]any{"m": 1.0, "c": 2.0, "x": 3.0, "a": 4.0}
	actual := map[string]any{"m": 0.0, "c": 0.0, "x": 0.0, "a": 0.0}
	var first string
	for i := 0; i < 100; i++ {
		err := MatchExpect(expect, actual, NewRun().NewScope())
		if err == nil {
			t.Fatal("mismatch passed")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("run %d reported %q, run 0 reported %q", i, err, first)
		}
	}
	if !strings.Contains(first, `"a"`) {
		t.Errorf("first failure = %q, want the first field by name", first)
	}
}

// regex renders a numeric response value the way interpolation does: a JSON
// number decodes as float64, and fmt.Sprint wrote 1234567 as 1.234567e+06,
// so `^\d+$` failed against an integer id.
func TestRegex_NumericValueHasNoExponent(t *testing.T) {
	cases := map[string]float64{`^\d+$`: 1234567, `^12345678901$`: 12345678901, `^0\.000001$`: 0.000001}
	for pattern, v := range cases {
		m, err := DecodeMatcher(map[string]any{"matcher": "regex", "pattern": pattern})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Match(v, true, NewRun().NewScope()); err != nil {
			t.Errorf("regex %s against %v: %v", pattern, v, err)
		}
	}
}

// A fork tries a match without binding anything in its parent; Adopt keeps
// what the fork bound.
func TestScope_ForkAndAdopt(t *testing.T) {
	s := NewRun().NewScope()
	s.Capture("a", 1)
	f := s.Fork()
	f.Capture("b", 2)
	if _, ok := s.Get("b"); ok {
		t.Fatal("a fork's capture leaked into its parent")
	}
	s.Adopt(f)
	if v, ok := s.Get("b"); !ok || v != 2 {
		t.Fatalf("adopted capture = %v, %v", v, ok)
	}
	if v, _ := s.Get("a"); v != 1 {
		t.Fatalf("parent capture lost: %v", v)
	}
}
