package migratecli

import (
	"strings"
	"testing"
)

// --allow-no-dsn is read only by `fixtures apply`. On every other verb it used
// to be accepted and ignored, which reads as "apply may proceed without a DSN"
// — the one thing apply never does.
func TestParseFlags_AllowNoDSNOnlyOnFixtures(t *testing.T) {
	for _, verb := range []string{"apply", "status", "rollback", "fetch"} {
		_, err := parseFlags(verb, []string{"--allow-no-dsn"}, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "applies only to `fixtures apply`") {
			t.Errorf("%s --allow-no-dsn: want a refusal naming fixtures apply, got %v", verb, err)
		}
		if _, err := parseFlags(verb, nil, &strings.Builder{}); err != nil {
			t.Errorf("%s without the flag: %v", verb, err)
		}
	}
	f, err := parseFlags("fixtures apply", []string{"--allow-no-dsn"}, &strings.Builder{})
	if err != nil || !f.allowNoDSN {
		t.Fatalf("fixtures apply --allow-no-dsn: allow=%v err=%v", f.allowNoDSN, err)
	}
}
