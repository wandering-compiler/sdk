package eventbus_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		filter string
		topic  string
		want   bool
		why    string
	}{
		// Exact literal matches.
		{"user.created", "user.created", true, "exact match"},
		{"user.created", "user.deleted", false, "different leaf"},
		{"user.created", "user.created.extra", false, "literal doesn't consume tail"},

		// Single-segment wildcard.
		{"user.*", "user.created", true, "* matches one segment"},
		{"user.*", "user.deleted", true, "* matches any single segment"},
		{"user.*", "user.created.email", false, "* doesn't match multi-segment"},
		{"user.*", "user", false, "* requires a segment to consume"},
		{"*.created", "user.created", true, "* in non-terminal position"},
		{"*.*", "user.created", true, "two single-segment wildcards"},

		// Multi-segment wildcard.
		{"**", "anything", true, "** matches one"},
		{"**", "any.thing.here", true, "** matches many"},
		{"**", "", true, "** matches zero (empty topic)"},
		{"user.**", "user.created", true, "** matches one tail segment"},
		{"user.**", "user.created.email", true, "** matches multi-segment tail"},
		{"user.**", "user", true, "** matches zero tail segments"},
		{"user.**", "billing.created", false, "** doesn't change literal prefix"},
		{"**.created", "user.profile.created", true, "** in front matches multi"},
		{"**.created", "user.deleted", false, "** in front + literal must match leaf"},

		// Mixed wildcards.
		{"user.*.email", "user.profile.email", true, "* between literals"},
		{"user.**.email", "user.a.b.email", true, "** between literals"},
		{"user.**.email", "user.email", true, "** between literals matches zero"},
		{"user.**.email", "user.a.b.notemail", false, "** between but suffix mismatch"},

		// Empty edge cases.
		{"", "", true, "both empty"},
		{"", "anything", false, "empty filter rejects non-empty topic"},
		{"**", "", true, "** matches empty topic"},

		// Defensive cases.
		{"a.b.c", "a.b", false, "filter longer than topic without **"},
		{"a.b", "a.b.c", false, "topic longer than filter without **"},
	}
	for _, c := range cases {
		if got := eventbus.MatchGlob(c.filter, c.topic); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v — %s", c.filter, c.topic, got, c.want, c.why)
		}
	}
}
