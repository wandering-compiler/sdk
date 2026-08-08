package sqlite_test

import (
	"context"
	"database/sql"
	_ "modernc.org/sqlite"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/sqlitecollate"
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
	// The path is the subject here; the session pragmas ride along on
	// every DSN this builder produces (B-F7) and are pinned by
	// TestURLToDriverDSN_CarriesTheRuntimeSessionPragmas.
	if path, _, _ := strings.Cut(got, "?"); path != "/abs/path.db" {
		t.Errorf("got %q, want path /abs/path.db", got)
	}
}

func TestURLToDriverDSN_StripsRelative(t *testing.T) {
	got := sqlite.URLToDriverDSN("sqlite://relative.db")
	// The path is the subject here; the session pragmas ride along on
	// every DSN this builder produces (B-F7) and are pinned by
	// TestURLToDriverDSN_CarriesTheRuntimeSessionPragmas.
	if path, _, _ := strings.Cut(got, "?"); path != "relative.db" {
		t.Errorf("got %q, want path relative.db", got)
	}
}

func TestURLToDriverDSN_FilePassThrough(t *testing.T) {
	got := sqlite.URLToDriverDSN("file:test.db?mode=rwc")
	// "Pass through" is about the FORM: the driver-native shape is not
	// rewritten. The session pragmas are still appended — a connection
	// opened this way runs the same SQL as every other one, so it must
	// mean the same thing (B-F7).
	if !strings.HasPrefix(got, "file:test.db?mode=rwc") {
		t.Errorf("file: form should pass through unchanged; got %q", got)
	}
	if !strings.Contains(got, "_pragma=case_sensitive_like(1)") {
		t.Errorf("file: form lost the session pragmas; got %q", got)
	}
}

func TestURLToDriverDSN_BarePathPassThrough(t *testing.T) {
	got := sqlite.URLToDriverDSN("/tmp/raw.db")
	// The PATH passes through; the session pragmas are appended to it,
	// because a bare-path DSN opens the same kind of connection as any
	// other and has to mean the same thing (B-F7).
	if path, _, _ := strings.Cut(got, "?"); path != "/tmp/raw.db" {
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

// T1-4 pass #12, B-F7. The applier must open its connection with the
// same session as the generated runtime.
//
// The runtime pins `case_sensitive_like` on every pooled connection so a
// DQL `LIKE` means what it means on PostgreSQL. The applier registered
// the collation but never set the pragma, so LIKE was
// ASCII-case-insensitive at apply time — and LIKE is reachable there:
// `raw_checks.expr` and `raw_indexes.body` are documented opaque-SQL
// pass-throughs, a 12-step rebuild re-INSERTs every row (re-evaluating
// CHECKs) and recomputes partial-index membership, and data-migration SQL
// runs on this connection too. So a CHECK could accept at apply what the
// runtime rejects, and a partial index could hold a different row set
// than the queries reading it assume.
//
// Asserted through the DSN the applier actually opens with, so the test
// fails if the pragma stops riding the connection — not merely if a
// constant changes.
func TestURLToDriverDSN_CarriesTheRuntimeSessionPragmas(t *testing.T) {
	driverDSN := sqlite.URLToDriverDSN(filepath.Join(t.TempDir(), "session.db"))
	db, err := sql.Open("sqlite", driverDSN)
	if err != nil {
		t.Fatalf("open %q: %v", driverDSN, err)
	}
	defer func() { _ = db.Close() }()

	var like int
	if err := db.QueryRow(`SELECT 'ABC' LIKE 'a%'`).Scan(&like); err != nil {
		t.Fatalf("probe LIKE: %v", err)
	}
	if like != 0 {
		t.Errorf("applier LIKE is case-INsensitive (got %d) — the runtime pins case_sensitive_like, so a CHECK or partial index means something different at apply time; DSN was %q", like, driverDSN)
	}

	// The FK pragma the applier sets per migration must not regress;
	// carrying it on the DSN makes it the session default instead.
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("probe foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("applier session has foreign_keys OFF (got %d); DSN was %q", fk, driverDSN)
	}
}

// B-F5. The applier records which collator built this database's indexes,
// and refuses to add more DDL on top of a database built with a different
// one.
//
// sqlitecollate promises that the ordering an index was built with is the
// ordering a query compares with. That is true inside one BUILD, and the
// applier, the dev-DB snapshotter and every generated runtime are separate
// binaries: the x/text tables driving the collation and the Go toolchain
// tables driving the upper/lower UDFs move independently. A mismatch sorts
// an index one way and reads it another, with nothing to see.
//
// Refusal happens at APPLY because that is the moment the damage would be
// done — applying DDL under a foreign collator is what builds the
// mismatched index.
func TestApply_RefusesADatabaseBuiltWithAnotherCollator(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stamped.db")

	a, err := sqlite.New(ctx, path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "m1",
		UpSql: `CREATE TABLE t (id INTEGER PRIMARY KEY)`,
	}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	side, err := sql.Open("sqlite", sqlite.URLToDriverDSN(path))
	if err != nil {
		t.Fatalf("sibling open: %v", err)
	}
	defer func() { _ = side.Close() }()

	var stored string
	if err := side.QueryRow(`SELECT fingerprint FROM wc_collation WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("no collation stamp written: %v", err)
	}
	if stored != sqlitecollate.Fingerprint() {
		t.Errorf("stamp = %q, want this build's fingerprint %q", stored, sqlitecollate.Fingerprint())
	}

	// Re-applying under the SAME binary must stay allowed.
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "m2",
		UpSql: `CREATE TABLE t2 (id INTEGER PRIMARY KEY)`,
	}); err != nil {
		t.Fatalf("same-collator re-apply must be allowed: %v", err)
	}

	// Stand in for a binary built from different Unicode tables.
	if _, err := side.Exec(`UPDATE wc_collation SET fingerprint = ? WHERE id = 1`,
		"w17c1:9.0.0:9.0.0:deadbeefdeadbeef"); err != nil {
		t.Fatalf("rewrite stamp: %v", err)
	}
	err = a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "m3",
		UpSql: `CREATE TABLE t3 (id INTEGER PRIMARY KEY)`,
	})
	if err == nil {
		t.Fatal("apply proceeded onto a database built with a different collator — the index it creates would be sorted by one collator and read by another")
	}
	// The diagnostic has to say WHICH side moved, or the operator cannot act.
	for _, want := range []string{"9.0.0", sqlitecollate.Fingerprint()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}
