package dqlbind

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"

	"github.com/wandering-compiler/sdk/go/lib/dbsession"
)

func openLiveMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("W17_MYSQL_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_MYSQL_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_MYSQL_DSN not set — live MySQL test skipped")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mysqldrv.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	db, err := sql.Open("mysql", dbsession.ApplyMySQLDSNParams(cfg.FormatDSN()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func queryIDs(t *testing.T, db *sql.DB, query string, args ...any) []int {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	got := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return got
}

// The expanded statements run on a real MySQL server and mean what they
// say: a multi-row INSERT stores every row, two IN lists bind onto their own
// placeholders past a literal `?`, and the empty list matches nothing under
// IN and excludes nothing under NOT IN.
func TestExpandedSQL_RunsOnMySQL_Live(t *testing.T) {
	db := openLiveMySQL(t)
	table := fmt.Sprintf("dqlbind_%d", time.Now().UnixNano())
	if _, err := db.Exec("CREATE TABLE " + table + " (id INT PRIMARY KEY, note VARCHAR(16) NOT NULL, b INT NOT NULL, c INT NOT NULL)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + table) })

	insert := ExpandValuesQM("INSERT INTO "+table+" (id, note, b, c) VALUES (?, 'why?', ?, ?)", 3, 3)
	if _, err := db.Exec(insert, 1, 10, 20, 2, 20, 99, 3, 30, 30); err != nil {
		t.Fatalf("multi-row insert %q: %v", insert, err)
	}
	if got := queryIDs(t, db, "SELECT id FROM "+table+" WHERE note = 'why?' ORDER BY id"); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("rows stored = %v, want [1 2 3] (sql: %q)", got, insert)
	}

	l1, l2 := []any{10, 30}, []any{20, 99}
	query := "SELECT id FROM " + table + " WHERE note = 'why?' AND b IN (?) AND c IN (?) ORDER BY id"
	query = ExpandIn(query, 2, len(l2))
	query = ExpandIn(query, 1, len(l1))
	if got := queryIDs(t, db, query, append(append([]any{}, l1...), l2...)...); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("IN lists returned %v, want [1] (sql: %q)", got, query)
	}

	none := ExpandIn("SELECT id FROM "+table+" WHERE b IN (?) ORDER BY id", 1, 0)
	if got := queryIDs(t, db, none); len(got) != 0 {
		t.Errorf("an empty IN list matched %v (sql: %q)", got, none)
	}
	all := ExpandIn("SELECT id FROM "+table+" WHERE b NOT IN (?) ORDER BY id", 1, 0)
	if got := queryIDs(t, db, all); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Errorf("an empty NOT IN list returned %v, want every row (sql: %q)", got, all)
	}
}
