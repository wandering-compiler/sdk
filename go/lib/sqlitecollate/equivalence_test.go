package sqlitecollate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
)

// Equality is deterministic, as on PostgreSQL: only identical strings are
// equal. Canonically equivalent spellings and strings that differ only in
// ignorable characters are distinct values — in Compare, in `=`, and in a
// UNIQUE index — while ordering still follows the UCA.
func TestCompare_EqualityIsDeterministic(t *testing.T) {
	pairs := [][2]string{
		{"café", "café"},  // NFC vs NFD
		{"ab", "a\u200bb"}, // zero width space
		{"a", "a\x00"},     // NUL
	}
	for _, p := range pairs {
		if Compare(p[0], p[1]) == 0 || Compare(p[1], p[0]) != -Compare(p[0], p[1]) {
			t.Errorf("Compare(%q, %q) = %d, want a consistent non-zero order", p[0], p[1], Compare(p[0], p[1]))
		}
	}
	if Compare("x", "x") != 0 {
		t.Error("identical strings are not equal")
	}
	if Compare("cafe", "café") >= 0 || Compare("Adam", "adam") <= 0 && Compare("adam", "Adam") >= 0 {
		t.Error("the UCA order itself changed")
	}

	Register()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "eq.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE t (name TEXT COLLATE ` + Name + ` UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	for _, p := range pairs {
		for _, v := range p {
			if _, err := db.Exec(`INSERT INTO t VALUES (?)`, v); err != nil {
				t.Errorf("insert %q next to its look-alike: %v", v, err)
			}
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM t WHERE name = ?`, p[1]).Scan(&n); err != nil || n != 1 {
			t.Errorf("WHERE name = %q matched %d rows, %v; want exactly its own", p[1], n, err)
		}
	}
}

// The stamp a plain-UCA build wrote is this build's legacy fingerprint, so
// the applier can recognise it.
func TestLegacyFingerprint_IsThePlainUCABuildsStamp(t *testing.T) {
	if unicode.Version != "15.0.0" || norm.Version != "15.0.0" {
		t.Skipf("pinned value is for Unicode 15.0.0 tables; this build has %s / %s", unicode.Version, norm.Version)
	}
	if got := legacyFingerprint(); got != "w17c1:15.0.0:15.0.0:f59c8e08423b8fc2" {
		t.Errorf("legacyFingerprint() = %q, no longer what the plain-UCA build stamped", got)
	}
}

// A database stamped by a plain-UCA build is upgraded by the applier: its
// indexes are rebuilt and the stamp rewritten, so a look-alike that the old
// index ranked equal can be stored and found.
func TestEnsureStamp_UpgradesAPlainUCADatabase(t *testing.T) {
	Register()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "up.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TABLE ` + StampTable + ` (id INTEGER PRIMARY KEY CHECK (id = 1), fingerprint TEXT NOT NULL)`,
		`CREATE TABLE t (name TEXT COLLATE ` + Name + ` UNIQUE)`,
		`INSERT INTO t VALUES ('caf` + "é" + `'), ('ab')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO `+StampTable+` VALUES (1, ?)`, legacyFingerprint()); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStamp(ctx, db); err == nil {
		t.Error("the runtime accepted a database whose indexes were built by the old comparison")
	}
	if err := EnsureStamp(ctx, db); err != nil {
		t.Fatalf("EnsureStamp on a plain-UCA database: %v", err)
	}
	if err := VerifyStamp(ctx, db); err != nil {
		t.Errorf("after the upgrade: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (?), (?)`, "café", "a\u200bb"); err != nil {
		t.Fatalf("insert look-alikes after the upgrade: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t WHERE name = ?`, "café").Scan(&n); err != nil || n != 1 {
		t.Errorf("lookup after the upgrade matched %d, %v", n, err)
	}

	// A stamp from different tables is still a mismatch, not upgraded.
	if _, err := db.Exec(`UPDATE ` + StampTable + ` SET fingerprint = 'w17c1:9.0.0:9.0.0:deadbeefdeadbeef'`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStamp(ctx, db); err == nil {
		t.Error("a foreign stamp was accepted")
	}
}
