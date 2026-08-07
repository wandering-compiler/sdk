package fingerprint_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
)

// TestExtractSQLite_RoundTrip builds a known schema in an
// in-memory SQLite, extracts the fingerprint, asserts the
// canonical text representation has the expected shape.
func TestExtractSQLite_RoundTrip(t *testing.T) {
	db := sqliteMem(t)
	mustExec(t, db, `CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		email TEXT NOT NULL,
		full_name TEXT
	)`)
	mustExec(t, db, `CREATE TABLE wc_migrations (
		timestamp TEXT PRIMARY KEY,
		applied_at TEXT,
		content_sha256 BLOB
	)`)

	got, err := fingerprint.ExtractSQLite(context.Background(), db)
	if err != nil {
		t.Fatalf("ExtractSQLite: %v", err)
	}

	formatted := got.Format()
	if !strings.Contains(formatted, `TABLE "users"`) {
		t.Errorf("formatted output missing users; got:\n%s", formatted)
	}
	if strings.Contains(formatted, "wc_migrations") {
		t.Errorf("wc_migrations should be excluded; got:\n%s", formatted)
	}
	if !strings.Contains(formatted, `COLUMN "email"`) {
		t.Errorf("missing email column; got:\n%s", formatted)
	}
	if !strings.Contains(formatted, `COLUMN "full_name"`) || !strings.Contains(formatted, "null") {
		t.Errorf("full_name should be marked nullable; got:\n%s", formatted)
	}
}

// TestExtractSQLite_FingerprintIsDeterministic — same schema =
// same fingerprint hex.
func TestExtractSQLite_FingerprintIsDeterministic(t *testing.T) {
	db1 := sqliteMem(t)
	db2 := sqliteMem(t)
	mustExec(t, db1, `CREATE TABLE x (id INTEGER, name TEXT)`)
	mustExec(t, db2, `CREATE TABLE x (id INTEGER, name TEXT)`)

	a, err := fingerprint.ExtractSQLite(context.Background(), db1)
	if err != nil {
		t.Fatalf("Extract 1: %v", err)
	}
	b, err := fingerprint.ExtractSQLite(context.Background(), db2)
	if err != nil {
		t.Fatalf("Extract 2: %v", err)
	}
	if a.FingerprintHex() != b.FingerprintHex() {
		t.Errorf("identical schema should yield identical fingerprint:\n  a=%s\n  b=%s",
			a.FingerprintHex(), b.FingerprintHex())
	}
}

// TestExtractSQLite_ChangeAffectsFingerprint — adding a column
// changes the fingerprint.
func TestExtractSQLite_ChangeAffectsFingerprint(t *testing.T) {
	db := sqliteMem(t)
	mustExec(t, db, `CREATE TABLE x (id INTEGER)`)
	before, _ := fingerprint.ExtractSQLite(context.Background(), db)

	mustExec(t, db, `ALTER TABLE x ADD COLUMN name TEXT`)
	after, _ := fingerprint.ExtractSQLite(context.Background(), db)

	if before.FingerprintHex() == after.FingerprintHex() {
		t.Errorf("fingerprint should change after ALTER TABLE; got same hex %s",
			before.FingerprintHex())
	}
}

// TestExtractSQLite_EmptySchema — no user tables yields empty
// canonical text + a stable empty-input fingerprint.
func TestExtractSQLite_EmptySchema(t *testing.T) {
	db := sqliteMem(t)
	got, err := fingerprint.ExtractSQLite(context.Background(), db)
	if err != nil {
		t.Fatalf("ExtractSQLite: %v", err)
	}
	if got.Format() != "" {
		t.Errorf("empty schema should format to empty; got %q", got.Format())
	}
}

func sqliteMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
