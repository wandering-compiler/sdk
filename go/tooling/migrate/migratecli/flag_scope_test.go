package migratecli

import (
	"regexp"
	"strings"
	"testing"
)

// A flag takes effect on the verb it is given to, or that verb refuses it:
// a flag accepted and then ignored tells the operator nothing.
func TestParseFlags_AFlagTheVerbDoesNotReadIsRefused(t *testing.T) {
	for _, c := range []struct {
		verb string
		args []string
	}{
		{"status", []string{"--fake"}},
		{"status", []string{"--dry-run"}},
		{"apply", []string{"--to", "20260101T000000Z"}},
		{"apply", []string{"--domain", "billing"}},
		{"apply", []string{"--exit-code"}},
		{"rollback", []string{"--to", "", "--fake"}},
		{"fetch", []string{"--fetch"}},
		{"fixtures apply", []string{"--log-format", "json"}},
	} {
		_, err := parseFlags(c.verb, c.args, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "has no effect on `"+c.verb+"`") {
			t.Errorf("%s %v = %v, want a refusal naming the verb", c.verb, c.args, err)
		}
	}
	for _, c := range []struct {
		verb string
		args []string
	}{
		{"apply", []string{"--fake", "--log-format", "json", "--parallel", "2"}},
		{"status", []string{"--exit-code", "--fetch"}},
		{"rollback", []string{"--to", "", "--dry-run"}},
		{"fetch", []string{"--lock", "l.yaml", "--migrations", "m"}},
		{"fixtures apply", []string{"--domain", "d", "--group", "g", "--allow-stale"}},
	} {
		if _, err := parseFlags(c.verb, c.args, &strings.Builder{}); err != nil {
			t.Errorf("%s %v refused: %v", c.verb, c.args, err)
		}
	}
}

// Every flag the verbs share is in the table, with at least one verb that
// reads it: a flag nobody reads (as --schema was, after its verb went) has
// no place in the set.
func TestFlagVerbs_CoversEveryDefinedFlag(t *testing.T) {
	var help strings.Builder
	_, _ = parseFlags("apply", []string{"-h"}, &help)
	defined := regexp.MustCompile(`(?m)^\s+-([a-z][a-z-]*)`).FindAllStringSubmatch(help.String(), -1)
	if len(defined) == 0 {
		t.Fatalf("no flags read from the usage text: %q", help.String())
	}
	for _, m := range defined {
		if len(flagVerbs[m[1]]) == 0 {
			t.Errorf("--%s is defined but no verb is recorded as reading it", m[1])
		}
	}
	for name := range flagVerbs {
		if !strings.Contains(help.String(), "-"+name) {
			t.Errorf("flagVerbs lists --%s, which is not defined", name)
		}
	}
}
