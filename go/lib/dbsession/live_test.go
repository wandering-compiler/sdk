package dbsession_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/wandering-compiler/sdk/go/lib/dbsession"
)

// liveServer returns the DSN in env, or skips — failing instead when
// W17_REQUIRE_LIVE_MIGRATOR=1, so a CI lane that should reach a server
// cannot go green by skipping.
func liveServer(t *testing.T, env string) string {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatalf("W17_REQUIRE_LIVE_MIGRATOR=1 but %s is not set — refusing to skip-green", env)
		}
		t.Skipf("%s not set — live test skipped", env)
	}
	return dsn
}

// mysqlDriverDSN turns W17_MYSQL_DSN (mysql://user:pass@host:port/db) into
// the driver's own form, naming no session parameters.
func mysqlDriverDSN(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(liveServer(t, "W17_MYSQL_DSN"))
	if err != nil {
		t.Fatalf("W17_MYSQL_DSN: %v", err)
	}
	cfg := mysqldrv.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	return cfg.FormatDSN()
}

// The MySQL pins against a server whose own defaults are the ones they
// exist to override: NO_BACKSLASH_ESCAPES on, and the server's
// accent- and case-insensitive default collation.
func TestMySQLPins_LiveServer(t *testing.T) {
	bare := mysqlDriverDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := sql.Open("mysql", bare)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	// sql_mode has no per-database form, so the server's global mode is
	// changed for this test and restored after it.
	var mode string
	if err := admin.QueryRowContext(ctx, "SELECT @@GLOBAL.sql_mode").Scan(&mode); err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "SET GLOBAL sql_mode = CONCAT(@@GLOBAL.sql_mode, ',NO_BACKSLASH_ESCAPES')"); err != nil {
		t.Fatalf("set sql_mode: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "SET GLOBAL sql_mode = ?", mode) })

	db, err := sql.Open("mysql", dbsession.ApplyMySQLDSNParams(bare))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// The emitter writes a backslash in a literal as two.
	var s string
	if err := db.QueryRowContext(ctx, `SELECT 'a\\b'`).Scan(&s); err != nil {
		t.Fatalf("backslash literal: %v", err)
	}
	if s != `a\b` {
		t.Errorf(`'a\\b' read back as %q, want %q — backslash doubling no longer holds`, s, `a\b`)
	}

	var at time.Time
	if err := db.QueryRowContext(ctx, `SELECT CAST('2026-01-02 03:04:05' AS DATETIME)`).Scan(&at); err != nil {
		t.Fatalf("DATETIME into time.Time: %v", err)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !at.Equal(want) {
		t.Errorf("DATETIME = %v, want %v", at, want)
	}

	for _, c := range []struct{ a, b string }{{"a", "A"}, {"e", "é"}} {
		var eq int
		if err := db.QueryRowContext(ctx, `SELECT '`+c.a+`' = '`+c.b+`'`).Scan(&eq); err != nil {
			t.Fatalf("compare: %v", err)
		}
		if eq != 0 {
			t.Errorf("'%s' = '%s' is true; literal comparison must be case- and accent-sensitive", c.a, c.b)
		}
	}
}

// The PostgreSQL pins against a database whose own settings are the ones
// they exist to override: standard_conforming_strings off, and a
// search_path that does not start at public.
func TestPGPins_LiveServer(t *testing.T) {
	base := liveServer(t, "W17_PG_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("pins_%d", time.Now().UnixNano())
	for _, q := range []string{
		"CREATE DATABASE " + name,
		"ALTER DATABASE " + name + " SET standard_conforming_strings = off",
		"ALTER DATABASE " + name + " SET search_path = elsewhere, public",
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name

	setup, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = setup.Close() }()
	for _, q := range []string{
		"CREATE SCHEMA elsewhere",
		"CREATE TABLE public.books (title TEXT)",
		"INSERT INTO public.books VALUES ('public')",
		"CREATE TABLE elsewhere.books (title TEXT)",
		"INSERT INTO elsewhere.books VALUES ('elsewhere')",
	} {
		if _, err := setup.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	db, err := sql.Open("postgres", dbsession.ApplyPGDSNParams(u.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// The emitter doubles a single quote and leaves a backslash alone.
	var s string
	if err := db.QueryRowContext(ctx, `SELECT 'it''s a\b'`).Scan(&s); err != nil {
		t.Fatalf("quoted literal: %v", err)
	}
	if s != `it's a\b` {
		t.Errorf(`'it''s a\b' read back as %q, want %q`, s, `it's a\b`)
	}

	var from string
	if err := db.QueryRowContext(ctx, `SELECT title FROM books`).Scan(&from); err != nil {
		t.Fatalf("bare table name: %v", err)
	}
	if from != "public" {
		t.Errorf("bare table name resolved in %q, want public", from)
	}
}
