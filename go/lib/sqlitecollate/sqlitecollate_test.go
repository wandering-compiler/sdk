package sqlitecollate_test

import (
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/wandering-compiler/sdk/go/lib/sqlitecollate"
)

func TestRegister_UnicodeOrderingAndEquality(t *testing.T) {
	sqlitecollate.Register()
	sqlitecollate.Register() // idempotent — a second call must not panic

	// A file DB (not :memory:) so every pooled connection sees the same table.
	dbPath := filepath.Join(t.TempDir(), "collate.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// The column carries the collation, so bare ORDER BY / = use it (this is how
	// the migrator emits string columns).
	if _, err := db.Exec(`CREATE TABLE t(name TEXT COLLATE ` + sqlitecollate.Name + `)`); err != nil {
		t.Fatalf("create table with COLLATE %s: %v", sqlitecollate.Name, err)
	}
	for _, v := range []string{"Zoe", "adam", "Adam", "café", "cafe", "Bob"} {
		if _, err := db.Exec(`INSERT INTO t VALUES (?)`, v); err != nil {
			t.Fatalf("insert %q: %v", v, err)
		}
	}

	rows, err := db.Query(`SELECT name FROM t ORDER BY name`)
	if err != nil {
		t.Fatalf("order-by query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// UCA order: base letter first (a<b<c<z), case/accent as tiebreaks — NOT
	// BINARY, which would put every uppercase before every lowercase
	// ([Adam, Bob, Zoe, adam, cafe, café]).
	want := []string{"adam", "Adam", "Bob", "cafe", "café", "Zoe"}
	if len(got) != len(want) {
		t.Fatalf("ORDER BY name = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ORDER BY name = %v, want %v (UCA order)", got, want)
		}
	}

	// Equality stays accent+case sensitive (as_cs), like MySQL / PG canonical.
	count := func(q string, arg string) int {
		var n int
		if err := db.QueryRow(q, arg).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", arg, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM t WHERE name = ?`, "adam"); n != 1 {
		t.Errorf("name = 'adam' matched %d rows, want 1 (case-sensitive, must not match 'Adam')", n)
	}
	if n := count(`SELECT count(*) FROM t WHERE name = ?`, "ADAM"); n != 0 {
		t.Errorf("name = 'ADAM' matched %d rows, want 0", n)
	}
	if n := count(`SELECT count(*) FROM t WHERE name = ?`, "cafe"); n != 1 {
		t.Errorf("name = 'cafe' matched %d rows, want 1 (accent-sensitive, must not match 'café')", n)
	}
}

// F7-A-4 / F8-D-4: Register also overrides SQLite's ASCII-only upper()/lower()
// with Unicode-aware versions, so `upper('café')` = 'CAFÉ' (not 'CAFé') on every
// connection — the same fold the applier/snapshotter/runtime all now share.
func TestRegister_UnicodeUpperLower(t *testing.T) {
	sqlitecollate.Register()
	dbPath := filepath.Join(t.TempDir(), "fold.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	scalar := func(q string) (string, bool) {
		t.Helper()
		var s sql.NullString
		if err := db.QueryRow(q).Scan(&s); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return s.String, s.Valid
	}
	if got, _ := scalar(`SELECT upper('café')`); got != "CAFÉ" {
		t.Errorf("upper('café') = %q, want CAFÉ (builtin override not active)", got)
	}
	if got, _ := scalar(`SELECT lower('CAFÉ')`); got != "café" {
		t.Errorf("lower('CAFÉ') = %q, want café", got)
	}
	if _, valid := scalar(`SELECT upper(NULL)`); valid {
		t.Error("upper(NULL) should be NULL")
	}
	if got, valid := scalar(`SELECT upper(123)`); !valid || got != "123" {
		t.Errorf("upper(123) = %q valid=%v, want 123/true (non-text passes through)", got, valid)
	}
}

func TestFoldCase_TypeHandling(t *testing.T) {
	cases := []struct {
		name string
		in   driver.Value
		want driver.Value
	}{
		{"text folded", "café", "CAFÉ"},
		{"nil stays nil", nil, nil},
		{"int passes through", int64(123), int64(123)},
		{"blob passes through", []byte{0x61}, []byte{0x61}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sqlitecollate.FoldCase([]driver.Value{tc.in}, strings.ToUpper)
			if gotB, ok := got.([]byte); ok {
				if wantB, _ := tc.want.([]byte); string(gotB) != string(wantB) {
					t.Errorf("FoldCase(%v) = %v, want %v", tc.in, got, tc.want)
				}
				return
			}
			if got != tc.want {
				t.Errorf("FoldCase(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
	if got := sqlitecollate.FoldCase(nil, strings.ToUpper); got != nil {
		t.Errorf("FoldCase(no args) = %v, want nil", got)
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int // sign
	}{
		{"adam", "Adam", -1}, // same letters, lowercase sorts before uppercase in UCA root
		{"a", "B", -1},       // base letter 'a' < 'B' (BINARY would give +1)
		{"café", "cafe", +1}, // accented after base
		{"abc", "abc", 0},
	}
	for _, tc := range cases {
		got := sqlitecollate.Compare(tc.a, tc.b)
		sign := 0
		switch {
		case got < 0:
			sign = -1
		case got > 0:
			sign = +1
		}
		if sign != tc.want {
			t.Errorf("Compare(%q,%q) sign = %d, want %d", tc.a, tc.b, sign, tc.want)
		}
	}
}
