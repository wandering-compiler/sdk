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

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write fail") }

// Dump over a file that is not a valid SQLite database: sql.Open succeeds
// (lazy), but VACUUM INTO fails when it touches the corrupt header.
func TestDump_VacuumError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.db")
	if err := writeGarbageDB(path); err != nil {
		t.Fatal(err)
	}
	s := &Snapshotter{path: path}
	if err := s.Dump(context.Background(), &bytes.Buffer{}); err == nil {
		t.Fatal("want VACUUM INTO error on a corrupt DB file")
	}
}

// writeGarbageDB writes a file with a bogus SQLite header so VACUUM INTO
// fails when it reads the page structure.
func writeGarbageDB(path string) error {
	junk := bytes.Repeat([]byte("not a sqlite db header at all\x00"), 64)
	return os.WriteFile(path, junk, 0o644)
}

func newApplier(t *testing.T) *Applier {
	t.Helper()
	a, err := New(context.Background(), "file:"+filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// Apply: a non-empty up_post_tx runs as a second step (covers the post-tx
// branch + its slog.Warn), and invalid SQL in either body surfaces.
func TestApply_PostTxAndErrors(t *testing.T) {
	a := newApplier(t)
	ctx := context.Background()

	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:       "m1",
		UpSql:    "CREATE TABLE t(id INTEGER PRIMARY KEY);",
		UpPostTx: "CREATE INDEX t_id ON t(id);",
	}); err != nil {
		t.Fatalf("apply with post-tx: %v", err)
	}

	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: "NOT VALID SQL;"}); err == nil {
		t.Fatal("want up_sql error")
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		UpSql:    "SELECT 1;",
		UpPostTx: "ALSO NOT VALID;",
	}); err == nil {
		t.Fatal("want up_post_tx error")
	}
}

// Rollback: down_pre_tx + down_sql both run (covers both branches); invalid
// SQL in either surfaces.
func TestRollback_PreTxDownAndErrors(t *testing.T) {
	a := newApplier(t)
	ctx := context.Background()
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		UpSql: "CREATE TABLE t(id INTEGER PRIMARY KEY); CREATE TABLE u(id INTEGER);",
	}); err != nil {
		t.Fatal(err)
	}

	if err := a.Rollback(ctx, &applyfetchpb.Migration{
		DownPreTx: "DROP TABLE u;",
		DownSql:   "DROP TABLE t;",
	}); err != nil {
		t.Fatalf("rollback pre-tx + down: %v", err)
	}

	if err := a.Rollback(ctx, &applyfetchpb.Migration{DownPreTx: "BAD PRE;"}); err == nil {
		t.Fatal("want down_pre_tx error")
	}
	if err := a.Rollback(ctx, &applyfetchpb.Migration{DownSql: "BAD DOWN;"}); err == nil {
		t.Fatal("want down_sql error")
	}
}

// Wipe: drops every user table on a populated DB (covers the list + DROP
// loop happy path).
func TestWipe_DropsAllTables(t *testing.T) {
	a := newApplier(t)
	ctx := context.Background()
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		UpSql: "CREATE TABLE a(id INTEGER); CREATE TABLE b(id INTEGER);",
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Wipe(ctx); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	// A second wipe on the now-empty DB is a clean no-op.
	if err := a.Wipe(ctx); err != nil {
		t.Fatalf("wipe (empty): %v", err)
	}
}

// Dump: a real DB streams a non-empty image to w (happy path); a failing
// writer surfaces the stream error.
func TestDump_HappyAndCopyError(t *testing.T) {
	dir := t.TempDir()
	path := makeDB(t, dir)
	s := &Snapshotter{path: path}

	var buf bytes.Buffer
	if err := s.Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("Dump produced an empty image")
	}

	if err := s.Dump(context.Background(), errWriter{}); err == nil {
		t.Fatal("want stream error from a failing writer")
	}
}
