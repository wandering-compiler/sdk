package runtime

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A capture of a field the response does not carry must fail the step.
// Binding nil let a later step interpolate it as "<nil>" — an auth token
// captured that way rode every following call as `Bearer <nil>`.
func TestCapture_AbsentFieldFails(t *testing.T) {
	scope := NewRun().NewScope()
	err := MatchExpect(map[string]any{"token": map[string]any{"capture": "auth.token"}},
		map[string]any{"user_id": "u1"}, scope)
	if err == nil || !strings.Contains(err.Error(), "field absent") {
		t.Fatalf("capture of an absent field must fail naming the absence, got %v", err)
	}
	if _, ok := scope.Get("auth.token"); ok {
		t.Error("a failed capture must not bind anything")
	}

	// The check-and-bind form fails the same way, even with an inner
	// matcher that would itself accept absence.
	err = MatchExpect(map[string]any{"token": map[string]any{"capture": "t", "matcher": "absent"}},
		map[string]any{}, scope)
	if err == nil {
		t.Fatal("capture with an inner matcher must still refuse an absent field")
	}

	// A present value still binds.
	if err := MatchExpect(map[string]any{"token": map[string]any{"capture": "auth.token"}},
		map[string]any{"token": "tok"}, scope); err != nil {
		t.Fatal(err)
	}
	if v, _ := scope.Get("auth.token"); v != "tok" {
		t.Errorf("bound %v, want tok", v)
	}
}

// count, empty and distinct must not pass for a field the response does
// not have — the misspelt-name case passed every one of them.
func TestCollectionMatchers_MisspeltFieldFails(t *testing.T) {
	resp := map[string]any{"items": []any{map[string]any{"id": 1.0}, map[string]any{"id": 2.0}}}
	for name, spec := range map[string]any{
		"count == 0":    map[string]any{"matcher": "count", "op": "==", "value": 0},
		"count <= 1":    map[string]any{"matcher": "count", "op": "<=", "value": 1},
		"empty":         map[string]any{"matcher": "empty"},
		"distinct == 0": map[string]any{"matcher": "distinct", "field": "id", "op": "==", "value": 0},
		"eq null":       map[string]any{"matcher": "eq", "value": nil},
	} {
		err := MatchExpect(map[string]any{"itmes": spec}, resp, NewRun().NewScope())
		if err == nil {
			t.Errorf("%s passed on a misspelt field", name)
			continue
		}
		if !strings.Contains(err.Error(), "matcher: absent") {
			t.Errorf("%s: the failure must name the explicit absence matcher: %v", name, err)
		}
	}
}

// empty and count distinguish an empty field from an absent one; absent
// asserts absence and nothing else.
func TestAbsentMatcher(t *testing.T) {
	absent := map[string]any{"matcher": "absent"}
	if err := match(t, absent, nil, false); err != nil {
		t.Errorf("absent field should pass absent: %v", err)
	}
	for _, v := range []any{[]any{}, "", 0.0, []any{1.0}} {
		if err := match(t, absent, v, true); err == nil {
			t.Errorf("present %v should fail absent", v)
		}
	}
	if err := match(t, map[string]any{"matcher": "empty"}, []any{}, true); err != nil {
		t.Errorf("present empty list should pass empty: %v", err)
	}
	if _, err := DecodeMatcher(map[string]any{"matcher": "absent", "value": 0}); err == nil {
		t.Error("absent takes no other keys")
	}
}

// Every spec the runtime's own advice tells an author to write must be one
// DecodeMatcher accepts: the placeholder once recommended exactly the count
// that decode refuses as vacuous.
func TestMatcherAdvice_Decodes(t *testing.T) {
	var msgs []string
	msgs = append(msgs, unwrittenMatcher{}.Match(nil, false, nil).Error())
	_, err := DecodeMatcher(map[string]any{"matcher": "count", "op": ">=", "value": 0})
	if err == nil {
		t.Fatal("vacuous count must be refused")
	}
	msgs = append(msgs, err.Error())
	msgs = append(msgs, emptyMatcher{}.Match(nil, false, nil).Error())

	flow := regexp.MustCompile("`(\\{matcher:[^`]*\\})`")
	for _, msg := range msgs {
		found := flow.FindAllStringSubmatch(msg, -1)
		if len(found) == 0 {
			t.Errorf("no suggested spec found in: %s", msg)
		}
		for _, f := range found {
			var spec any
			if err := yaml.Unmarshal([]byte(f[1]), &spec); err != nil {
				t.Errorf("advice %s is not YAML: %v", f[1], err)
				continue
			}
			if _, err := DecodeMatcher(spec); err != nil {
				t.Errorf("advice %s is refused by decode: %v", f[1], err)
			}
		}
	}
	for _, a := range unwrittenAdvice {
		if !strings.Contains(msgs[0], a) {
			t.Errorf("placeholder message does not carry advice %s", a)
		}
	}
}
