package mysql

import (
	"slices"
	"strings"
	"testing"
)

// TestSnapshotter_ConnFlagsAndChildEnv pins the pure CLI-flag + env builders the
// mysqldump/mysql shell-outs rely on (no DB / no binary needed). connFlags emits
// --host/--port always and --user only when set; childEnv carries MYSQL_PWD
// (keeping the password off argv) only when a password is present.
func TestSnapshotter_ConnFlagsAndChildEnv(t *testing.T) {
	withCreds, err := NewSnapshotter("mysql://alice:s3cret@db.example:3307/shop")
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	flags := withCreds.connFlags()
	for _, want := range []string{"--host=db.example", "--port=3307", "--user=alice"} {
		if !slices.Contains(flags, want) {
			t.Errorf("connFlags %v missing %q", flags, want)
		}
	}
	env := withCreds.childEnv()
	if !hasPrefix(env, "MYSQL_PWD=s3cret") {
		t.Errorf("childEnv should carry MYSQL_PWD, got %v", env)
	}

	// No userinfo → no --user flag, no MYSQL_PWD, default port.
	noCreds, err := NewSnapshotter("mysql://db.example/shop")
	if err != nil {
		t.Fatalf("NewSnapshotter (no creds): %v", err)
	}
	flags = noCreds.connFlags()
	if slices.ContainsFunc(flags, func(s string) bool { return strings.HasPrefix(s, "--user=") }) {
		t.Errorf("connFlags should omit --user when no user, got %v", flags)
	}
	if !slices.Contains(flags, "--port=3306") {
		t.Errorf("connFlags should default port to 3306, got %v", flags)
	}
	if hasPrefix(noCreds.childEnv(), "MYSQL_PWD=") {
		t.Error("childEnv should omit MYSQL_PWD when no password")
	}
}

func hasPrefix(env []string, prefix string) bool {
	return slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, prefix) })
}
