package runtime

import (
	"strings"
	"testing"
)

// The placeholder must FAIL — that is its entire job. A test is warranted
// even for a function that only returns an error, because the failure mode
// this replaces was precisely an assertion nobody noticed could not fail.
func TestUnwrittenMatcher_AlwaysFails(t *testing.T) {
	cases := []struct {
		name    string
		actual  any
		present bool
	}{
		{"absent", nil, false},
		{"empty list", []any{}, true},
		{"populated list", []any{1, 2, 3}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := unwrittenMatcher{}.Match(tc.actual, tc.present, nil)
			if err == nil {
				t.Fatalf("the generated placeholder must fail for %v", tc.actual)
			}
			// The message has to say what to do, or it just moves the
			// confusion from "why is this green" to "why is this red".
			if !strings.Contains(err.Error(), "unwritten") {
				t.Errorf("the failure must name the matcher to replace:\n%s", err)
			}
			if !strings.Contains(err.Error(), "accept anything on purpose") {
				t.Errorf("the failure must offer the deliberate escape hatch:\n%s", err)
			}
		})
	}
}

// DecodeMatcher must accept it — a placeholder the decoder rejects would
// fail as "unknown matcher kind", which reads like a typo rather than an
// unfinished case.
func TestUnwrittenMatcher_Decodes(t *testing.T) {
	m, err := DecodeMatcher(map[string]any{"matcher": "unwritten"})
	if err != nil {
		t.Fatalf("DecodeMatcher: %v", err)
	}
	if _, ok := m.(unwrittenMatcher); !ok {
		t.Errorf("want unwrittenMatcher, got %T", m)
	}
}
