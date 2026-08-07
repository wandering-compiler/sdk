package mysql

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// Dump / Restore happy paths: point the client binaries at trivial stand-in
// commands so the exec.CommandContext wiring runs without a live MySQL.
func TestSnapshotterDumpRestore_StubBinaries(t *testing.T) {
	for _, bin := range []string{"/bin/echo", "/usr/bin/true"} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("stub binary %s absent: %v", bin, err)
		}
	}
	s, err := NewSnapshotter("mysql://alice:s3cret@db.example:3307/shop")
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	s.dumpBin = "/bin/echo"
	s.restoreBin = "/usr/bin/true"

	var buf bytes.Buffer
	if err := s.Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump with /bin/echo stub: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("Dump wrote nothing to w")
	}
	if err := s.Restore(context.Background(), strings.NewReader("DROP TABLE t;\n")); err != nil {
		t.Fatalf("Restore with /usr/bin/true stub: %v", err)
	}
}

// Dump / Restore error paths: a missing client binary surfaces a wrapped
// error naming the operation.
func TestSnapshotterDumpRestore_MissingBinary(t *testing.T) {
	s, err := NewSnapshotter("mysql://alice@db.example/shop")
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	s.dumpBin = "mysqldump_does_not_exist"
	s.restoreBin = "mysql_does_not_exist"
	if err := s.Dump(context.Background(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "Dump") {
		t.Fatalf("Dump missing-binary error = %v", err)
	}
	if err := s.Restore(context.Background(), strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "Restore") {
		t.Fatalf("Restore missing-binary error = %v", err)
	}
}
