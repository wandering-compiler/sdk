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
	err = MatchExpect(map[string]any{"token": map[string]any{"capture": "t", "matcher": "empty"}},
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
