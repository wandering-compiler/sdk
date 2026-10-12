package dqlbind

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func liveDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "list.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY, b INTEGER)",
		"INSERT INTO t (id, b) VALUES (1, 10), (2, 20), (3, NULL)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func ids(t *testing.T, db *sql.DB, query string, args ...any) []int {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var got []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	return got
}

// An empty list filters by "none of these": IN matches nothing, and NOT IN
// — written either way — excludes nothing, NULL rows included. `NOT IN
// (NULL)` matched no rows at all.
func TestExpandIn_EmptyListMeansNoneOfThese(t *testing.T) {
	db := liveDB(t)
	for query, want := range map[string]string{
		"SELECT id FROM t WHERE b IN (?) ORDER BY id":       "",
		"SELECT id FROM t WHERE b NOT IN (?) ORDER BY id":   "1,2,3",
		"SELECT id FROM t WHERE NOT (b IN (?)) ORDER BY id": "1,2,3",
	} {
		got := ids(t, db, ExpandIn(query, 1, 0))
		var s []string
		for _, id := range got {
			s = append(s, strconv.Itoa(id))
		}
		if strings.Join(s, ",") != want {
			t.Errorf("%s with an empty list = [%s], want [%s]", query, strings.Join(s, ","), want)
		}
	}
}

// A list that would take the statement past the bind limit is refused
// before the statement is built — not passed to the driver, where it became
// an error that reads like a server fault.
func TestExpandInChecked_RefusesPastTheBindLimit(t *testing.T) {
	db := liveDB(t)
	const query = "SELECT id FROM t WHERE id = ? AND b IN (?)"

	// The limit is real: the unchecked expansion fails in the driver.
	n := MaxPlaceholders + 10
	args := make([]any, n+1)
	for i := range args {
		args[i] = i
	}
	if _, err := db.Query(ExpandIn(query, 2, n), args...); err == nil {
		t.Fatalf("the driver accepted %d placeholders; MaxPlaceholders is not a real limit", n+1)
	}

	if _, err := ExpandInChecked(query, 2, MaxPlaceholders); !errors.Is(err, ErrTooManyValues) {
		t.Errorf("one placeholder already present + %d = %v, want ErrTooManyValues", MaxPlaceholders, err)
	}
	got, err := ExpandInChecked(query, 2, MaxPlaceholders-1)
	if err != nil || countQM(got) != MaxPlaceholders {
		t.Fatalf("exactly at the limit: %d placeholders, %v", countQM(got), err)
	}
	if _, err := db.Query(got, args[:MaxPlaceholders]...); err != nil {
		t.Errorf("the driver refused a statement at the limit: %v", err)
	}
	if got, err := ExpandInChecked(query, 2, 0); err != nil || !strings.Contains(got, EmptyList) {
		t.Errorf("empty list: %q, %v", got, err)
	}

	// Multi-row INSERT is the same limit.
	if _, err := ExpandValuesQMChecked("INSERT INTO t (id, b) VALUES (?, ?)", 2, MaxPlaceholders/2+1); !errors.Is(err, ErrTooManyValues) {
		t.Errorf("QM VALUES past the limit = %v", err)
	}
	if _, err := ExpandValuesPGChecked("INSERT INTO t (id, b) VALUES ($1, $2)", 2, MaxPlaceholders/2+1); !errors.Is(err, ErrTooManyValues) {
		t.Errorf("PG VALUES past the limit = %v", err)
	}
	if got, err := ExpandValuesPGChecked("INSERT INTO t (id, b) VALUES ($1, '$9999999')", 1, 3); err != nil || got != "INSERT INTO t (id, b) VALUES ($1, '$9999999'), ($2, '$9999999'), ($3, '$9999999')" {
		t.Errorf("PG VALUES within the limit: %q, %v (a $k inside a literal is data)", got, err)
	}
}
