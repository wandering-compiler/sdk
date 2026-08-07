package postgres

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// conn() password routing: a URL DSN carrying a password strips it from
// the returned dbname (so it never lands on the client argv) and routes it
// via PGPASSWORD in the child env. Every other DSN form passes through with
// a nil env (the child inherits os.Environ()).
func TestSnapshotterConn(t *testing.T) {
	t.Run("url with password strips + routes PGPASSWORD", func(t *testing.T) {
		s := &Snapshotter{dsn: "postgres://alice:s3cret@db.local:5432/app"}
		dbname, env := s.conn()
		if strings.Contains(dbname, "s3cret") {
			t.Errorf("password leaked into dbname: %q", dbname)
		}
		if !strings.Contains(dbname, "alice@db.local") {
			t.Errorf("username/host not preserved: %q", dbname)
		}
		var found bool
		for _, kv := range env {
			if kv == "PGPASSWORD=s3cret" {
				found = true
			}
		}
		if !found {
			t.Errorf("PGPASSWORD not set in child env: %v", env)
		}
	})

	t.Run("url without password passes through, nil env", func(t *testing.T) {
		s := &Snapshotter{dsn: "postgres://bob@db.local:5432/app"}
		dbname, env := s.conn()
		if dbname != s.dsn {
			t.Errorf("dbname = %q, want pass-through %q", dbname, s.dsn)
		}
		if env != nil {
			t.Errorf("env = %v, want nil (inherit)", env)
		}
	})

	t.Run("libpq keyword/value DSN passes through", func(t *testing.T) {
		s := &Snapshotter{dsn: "host=db.local user=bob dbname=app"}
		dbname, env := s.conn()
		if dbname != s.dsn || env != nil {
			t.Errorf("conn() = (%q,%v), want pass-through nil-env", dbname, env)
		}
	})
}

// Dump / Restore happy paths: point the client binaries at trivial stand-in
// commands so the round-trip exercises the exec.CommandContext wiring
// without a live DB. `true` exits 0; `cat` echoes for Dump's stdout writer.
func TestSnapshotterDumpRestore_StubBinaries(t *testing.T) {
	s := &Snapshotter{dsn: "postgres://x@h/d", dumpBin: "/bin/echo", restoreBin: "/usr/bin/true"}

	var buf bytes.Buffer
	if err := s.Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump with /bin/echo stub: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("Dump wrote nothing to w")
	}
	if err := s.Restore(context.Background(), strings.NewReader("SELECT 1;\n")); err != nil {
		t.Fatalf("Restore with /usr/bin/true stub: %v", err)
	}
}

// Dump / Restore error paths: a missing client binary surfaces a wrapped
// error naming the binary.
func TestSnapshotterDumpRestore_MissingBinary(t *testing.T) {
	s := &Snapshotter{dsn: "postgres://x@h/d", dumpBin: "pg_dump_does_not_exist", restoreBin: "psql_does_not_exist"}
	if err := s.Dump(context.Background(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "Dump") {
		t.Fatalf("Dump missing-binary error = %v", err)
	}
	if err := s.Restore(context.Background(), strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "Restore") {
		t.Fatalf("Restore missing-binary error = %v", err)
	}
}

// Guard the stub binaries the happy-path test relies on actually exist on
// this host; skip rather than spuriously fail on an exotic layout.
func TestStubBinariesPresent(t *testing.T) {
	for _, bin := range []string{"/bin/echo", "/usr/bin/true"} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("stub binary %s absent: %v", bin, err)
		}
	}
}
