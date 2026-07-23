package sqlite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read fail") }

func TestNewSnapshotter_PathlessDSN(t *testing.T) {
	// "file:" is not in-memory but reduces to an empty path.
	if _, err := NewSnapshotter("file:"); err == nil {
		t.Fatal("want error for a pathless DSN")
	}
}

func TestDSNToFilePath_StripsQuery(t *testing.T) {
	got := dsnToFilePath("file:/tmp/x.db?cache=shared&mode=rwc")
	if got != "/tmp/x.db" {
		t.Errorf("dsnToFilePath = %q, want /tmp/x.db", got)
	}
}

// makeDB creates a real on-disk sqlite DB with one row and returns its path.
func makeDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "data.db")
	a, err := New(context.Background(), "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), &applyfetchpb.Migration{
		UpSql: "BEGIN; CREATE TABLE t(id INTEGER PRIMARY KEY); INSERT INTO t VALUES (1); COMMIT;",
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDump_CreateTempError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses dir permissions")
	}
	dir := t.TempDir()
	path := makeDB(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil { // read-only → CreateTemp fails
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s := &Snapshotter{path: path}
	if err := s.Dump(context.Background(), &bytes.Buffer{}); err == nil {
		t.Fatal("want error when the snapshot temp image can't be created")
	}
}

func TestRestore_MkdirError(t *testing.T) {
	base := t.TempDir()
	fileSlot := filepath.Join(base, "afile")
	if err := os.WriteFile(fileSlot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// path's dir is a regular file → MkdirAll fails.
	s := &Snapshotter{path: filepath.Join(fileSlot, "sub", "db")}
	if err := s.Restore(context.Background(), bytes.NewReader([]byte("data"))); err == nil {
		t.Fatal("want mkdir error")
	}
}

func TestRestore_CopyError(t *testing.T) {
	s := &Snapshotter{path: filepath.Join(t.TempDir(), "db")}
	if err := s.Restore(context.Background(), errReader{}); err == nil {
		t.Fatal("want copy error from a failing reader")
	}
}

func TestRestore_RenameError(t *testing.T) {
	dir := t.TempDir()
	// Make the destination path an existing directory → rename-into-place fails.
	dst := filepath.Join(dir, "db")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Snapshotter{path: dst}
	if err := s.Restore(context.Background(), bytes.NewReader([]byte("data"))); err == nil {
		t.Fatal("want rename error when the destination is a directory")
	}
}

// TestRestore_ClearsSidecars — a stale -wal/-shm beside the DB is removed.
func TestRestore_ClearsSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	for _, sc := range []string{path + "-wal", path + "-shm"} {
		if err := os.WriteFile(sc, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Snapshotter{path: path}
	if err := s.Restore(context.Background(), bytes.NewReader([]byte("image"))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, sc := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sc); !os.IsNotExist(err) {
			t.Errorf("sidecar %s not removed", sc)
		}
	}
}

// TestRestore_SidecarRemoveError — a `-wal` sidecar that is a non-empty
// directory can't be removed, surfacing the clear-sidecar error arm.
func TestRestore_SidecarRemoveError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	walDir := path + "-wal"
	if err := os.Mkdir(walDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A child makes the directory non-empty → os.Remove fails with ENOTEMPTY.
	if err := os.WriteFile(filepath.Join(walDir, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Snapshotter{path: path}
	if err := s.Restore(context.Background(), bytes.NewReader([]byte("image"))); err == nil {
		t.Fatal("want error when a sidecar can't be cleared")
	}
}

func TestWipe_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	path := makeDB(t, dir)
	a, err := New(context.Background(), "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled → acquiring a conn fails
	if err := a.Wipe(ctx); err == nil {
		t.Fatal("want error wiping with a cancelled context")
	}
}
