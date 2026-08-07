package mysql_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

func TestNewSnapshotter_Guards(t *testing.T) {
	cases := map[string]string{ // dsn → expected error substring
		"":                            "dsn is empty",
		"redis://h/x":                 "expected mysql:// scheme",
		"mysql:///justdb":             "missing host",
		"mysql://u:p@127.0.0.1:3306/": "missing database name",
	}
	for dsn, want := range cases {
		t.Run(dsn, func(t *testing.T) {
			_, err := mysql.NewSnapshotter(dsn)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("dsn %q: want error containing %q, got %v", dsn, want, err)
			}
		})
	}
}

// TestNewSnapshotter_OK — a well-formed DSN constructs (no dial; the
// client binaries connect when Dump/Restore runs). Default port is
// filled when omitted.
func TestNewSnapshotter_OK(t *testing.T) {
	for _, dsn := range []string{
		"mysql://u:p@127.0.0.1:3306/app",
		"mysql://u@db.local/app", // default port
	} {
		if _, err := mysql.NewSnapshotter(dsn); err != nil {
			t.Errorf("dsn %q: %v", dsn, err)
		}
	}
}
