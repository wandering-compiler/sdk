package dialect

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// SQLite tests use a real in-process DB (cheap — no
// container, no network). Hits modernc.org/sqlite as the
// production driver. Tests live in `dialect_test` package
// internally to access unexported helpers.

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("pragma fk: %v", err)
	}
	return db
}

func TestParseSQLite_UniqueSingle(t *testing.T) {
	db := openSQLite(t)
	mustExec(t, db, `CREATE TABLE u (
		id INTEGER PRIMARY KEY,
		email TEXT NOT NULL,
		CONSTRAINT u_email_unique UNIQUE (email)
	)`)
	mustExec(t, db, `INSERT INTO u (email) VALUES ('a@b.c')`)
	_, err := db.Exec(`INSERT INTO u (email) VALUES ('a@b.c')`)
	ce, ok := ParseSQLite(err)
	if !ok || ce.Kind != KindUnique {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	// SQLite reports columns, not constraint name.
	if ce.Name != "" {
		t.Errorf("Name expected empty (SQLite quirk), got %q", ce.Name)
	}
	if ce.Table != "u" {
		t.Errorf("Table = %q, want %q", ce.Table, "u")
	}
	if len(ce.Columns) != 1 || ce.Columns[0] != "email" {
		t.Errorf("Columns = %v, want [email]", ce.Columns)
	}
}

func TestParseSQLite_UniqueComposite(t *testing.T) {
	db := openSQLite(t)
	mustExec(t, db, `CREATE TABLE pairs (
		a TEXT, b TEXT,
		CONSTRAINT pairs_ab_unique UNIQUE (a, b)
	)`)
	mustExec(t, db, `INSERT INTO pairs (a, b) VALUES ('x', 'y')`)
	_, err := db.Exec(`INSERT INTO pairs (a, b) VALUES ('x', 'y')`)
	ce, ok := ParseSQLite(err)
	if !ok || ce.Kind != KindUnique {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if ce.Table != "pairs" {
		t.Errorf("Table = %q", ce.Table)
	}
	if len(ce.Columns) != 2 || ce.Columns[0] != "a" || ce.Columns[1] != "b" {
		t.Errorf("Columns = %v, want [a b]", ce.Columns)
	}
}

func TestParseSQLite_FK_Degraded(t *testing.T) {
	// SQLite FK violations expose NOTHING — proves the
	// degraded case from the decision doc. Wrap relies on
	// the registry's sole-FK heuristic to attribute.
	db := openSQLite(t)
	mustExec(t, db, `CREATE TABLE parent (id INTEGER PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE child (
		id INTEGER PRIMARY KEY,
		pid INTEGER NOT NULL,
		CONSTRAINT child_parent_fk FOREIGN KEY (pid) REFERENCES parent(id)
	)`)
	_, err := db.Exec(`INSERT INTO child (pid) VALUES (99)`)
	ce, ok := ParseSQLite(err)
	if !ok || ce.Kind != KindFK {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if ce.Name != "" || ce.Table != "" || len(ce.Columns) != 0 {
		t.Errorf("FK should be empty across the board, got %+v", ce)
	}
}

func TestParseSQLite_Check(t *testing.T) {
	db := openSQLite(t)
	mustExec(t, db, `CREATE TABLE c (
		age INTEGER,
		CONSTRAINT c_age_check CHECK (age >= 0)
	)`)
	_, err := db.Exec(`INSERT INTO c (age) VALUES (-1)`)
	ce, ok := ParseSQLite(err)
	if !ok || ce.Kind != KindCheck {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if ce.Name != "c_age_check" {
		t.Errorf("Name = %q", ce.Name)
	}
}

func TestParseSQLite_NotNull(t *testing.T) {
	db := openSQLite(t)
	mustExec(t, db, `CREATE TABLE n (
		id INTEGER PRIMARY KEY,
		email TEXT NOT NULL
	)`)
	_, err := db.Exec(`INSERT INTO n (id, email) VALUES (1, NULL)`)
	ce, ok := ParseSQLite(err)
	if !ok || ce.Kind != KindNotNull {
		t.Fatalf("got %+v ok=%v", ce, ok)
	}
	if ce.Table != "n" {
		t.Errorf("Table = %q", ce.Table)
	}
	if len(ce.Columns) != 1 || ce.Columns[0] != "email" {
		t.Errorf("Columns = %v", ce.Columns)
	}
}

func TestParseSQLite_NotSQLiteError(t *testing.T) {
	if _, ok := ParseSQLite(errors.New("not sqlite")); ok {
		t.Error("plain error should fall through")
	}
}

// Q55-grpcerr-2 — a mixed column list (different table prefixes, or a
// bare column among prefixed ones) cannot be attributed to one table:
// parseSQLiteColumnList must return an empty table, not retain the
// first prefix.
func TestParseSQLiteColumnList_MixedPrefix(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantTable string
		wantCols  []string
	}{
		{"single prefixed", "t.a", "t", []string{"a"}},
		{"same prefix", "t.a, t.b", "t", []string{"a", "b"}},
		{"different prefixes", "t.a, u.b", "", []string{"a", "b"}},
		{"bare after prefixed", "t.a, b", "", []string{"a", "b"}},
		{"prefixed after bare", "a, t.b", "", []string{"a", "b"}},
		{"all bare", "a, b", "", []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTable, gotCols := parseSQLiteColumnList(tc.in)
			if gotTable != tc.wantTable {
				t.Errorf("table = %q, want %q", gotTable, tc.wantTable)
			}
			if strings.Join(gotCols, ",") != strings.Join(tc.wantCols, ",") {
				t.Errorf("cols = %v, want %v", gotCols, tc.wantCols)
			}
		})
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
