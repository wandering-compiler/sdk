package dbsession_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/dbsession"
)

// The pins do on a live connection what they promise: foreign keys are
// enforced, LIKE is case-sensitive, and a stored time.Time is text SQLite's
// date functions parse. An operator's explicit foreign_keys(0) is what the
// connection then runs with.
func TestApplySQLitePragmas_LiveConnection(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "a.db")
	db, err := sql.Open("sqlite", dbsession.ApplySQLitePragmas(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var fk, like int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", fk, err)
	}
	if err := db.QueryRow(`SELECT 'a' LIKE 'A'`).Scan(&like); err != nil || like != 0 {
		t.Errorf("'a' LIKE 'A' = %d, %v; want 0 (case-sensitive)", like, err)
	}
	if _, err := db.Exec(`CREATE TABLE t (at TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (?)`, time.Date(2026, 8, 11, 12, 34, 56, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	var day sql.NullString
	if err := db.QueryRow(`SELECT date(at) FROM t`).Scan(&day); err != nil || day.String != "2026-08-11" {
		t.Errorf("date(stored time) = %q, %v; want 2026-08-11", day.String, err)
	}

	op, err := sql.Open("sqlite", dbsession.ApplySQLitePragmas(dsn+"?_pragma=foreign_keys(0)"))
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	if err := op.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 0 {
		t.Errorf("operator foreign_keys(0): connection runs with %d, %v; want 0", fk, err)
	}
}
