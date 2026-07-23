package sqlite_test

import (
	"context"
	"database/sql"
	_ "modernc.org/sqlite"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

func TestNew_EmptyDSNRefuses(t *testing.T) {
	_, err := sqlite.New(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "dsn is empty") {
		t.Errorf("expected empty-dsn error, got %v", err)
	}
}

// TestNew_OpensTempFile — happy path against a file in a temp
// dir; modernc.org/sqlite creates the DB on first connect.
func TestNew_OpensTempFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestNew_FilePrefix — `file:` form passes through to the
// driver unchanged; modernc.org/sqlite accepts it natively.
func TestNew_FilePrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	a, err := sqlite.New(context.Background(), "file:"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
}

// TestApply_HappyPath — open a temp DB, apply a CREATE TABLE
// migration, close. SQLite is in-process so no Docker needed.
func TestApply_HappyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	err = a.Apply(context.Background(), &applyfetchpb.Migration{
		Id:    "ts-1",
		UpSql: "BEGIN; CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT); COMMIT;",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// TestApply_WithPostTx — up_post_tx body runs after up_sql.
// SQLite doesn't have CREATE INDEX CONCURRENTLY but the
// orchestrator-level distinction still applies — exercise the
// branch.
func TestApply_WithPostTx(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	err = a.Apply(context.Background(), &applyfetchpb.Migration{
		Id:       "ts-1",
		UpSql:    "BEGIN; CREATE TABLE x (id INTEGER); COMMIT;",
		UpPostTx: "CREATE INDEX x_id ON x(id);",
	})
	if err != nil {
		t.Fatalf("Apply with post-tx: %v", err)
	}
}

// TestApply_BadSQLPropagates — malformed SQL surfaces verbatim.
func TestApply_BadSQLPropagates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	err = a.Apply(context.Background(), &applyfetchpb.Migration{
		Id: "bad", UpSql: "NOT A VALID SQL STATEMENT",
	})
	if err == nil {
		t.Fatal("expected SQL syntax error")
	}
	if !strings.Contains(err.Error(), "sqlite apply up_sql") {
		t.Errorf("err %q missing sqlite-up_sql prefix", err.Error())
	}
}

func TestApply_BadPostTxPropagates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	err = a.Apply(context.Background(), &applyfetchpb.Migration{
		Id:       "bad",
		UpSql:    "SELECT 1;", // ok
		UpPostTx: "SYNTAX ERROR HERE;",
	})
	if err == nil {
		t.Fatal("expected post-tx syntax error")
	}
	if !strings.Contains(err.Error(), "sqlite apply up_post_tx") {
		t.Errorf("err %q missing sqlite-up_post_tx prefix", err.Error())
	}
}

func TestClose_DoubleClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Second Close on a nil-conn applier returns nil — defensive
	// guard for production callers that may double-close on
	// error paths.
	if err := a.Close(); err != nil {
		t.Errorf("second Close: %v, want nil", err)
	}
}

func TestURLToDriverDSN_StripsAbsolute(t *testing.T) {
	got := sqlite.URLToDriverDSN("sqlite:///abs/path.db")
	if got != "/abs/path.db" {
		t.Errorf("got %q, want /abs/path.db", got)
	}
}

func TestURLToDriverDSN_StripsRelative(t *testing.T) {
	got := sqlite.URLToDriverDSN("sqlite://relative.db")
	if got != "relative.db" {
		t.Errorf("got %q, want relative.db", got)
	}
}

func TestURLToDriverDSN_FilePassThrough(t *testing.T) {
	got := sqlite.URLToDriverDSN("file:test.db?mode=rwc")
	if got != "file:test.db?mode=rwc" {
		t.Errorf("file: form should pass through unchanged; got %q", got)
	}
}

func TestURLToDriverDSN_BarePathPassThrough(t *testing.T) {
	got := sqlite.URLToDriverDSN("/tmp/raw.db")
	if got != "/tmp/raw.db" {
		t.Errorf("bare path should pass through; got %q", got)
	}
}

// TestWipe drops all user tables (migrate.Wiper). Pure-Go, no docker.
func TestWipe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, err := sqlite.New(ctx, "sqlite://"+dir+"/w.db")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	reg := &applyfetchpb.Migration{UpSql: `CREATE TABLE a(id INTEGER PRIMARY KEY); CREATE TABLE b(id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id)); INSERT INTO a VALUES (1);`}
	if err := a.Apply(ctx, reg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := a.Wipe(ctx); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	// Re-open + count user tables → 0.
	db, _ := sql.Open("sqlite", "file:"+dir+"/w.db")
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("after Wipe %d user tables remain, want 0", n)
	}
}
