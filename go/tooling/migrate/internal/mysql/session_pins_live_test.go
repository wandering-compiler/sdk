package mysql

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// Every connection the applier's pool opens carries the shared session
// pins, read back from the server: the accent- and case-sensitive
// collation, and backslash escapes left on.
func TestNew_EveryPooledConnectionCarriesTheSessionPins_Live(t *testing.T) {
	dsn := os.Getenv("W17_MYSQL_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_MYSQL_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_MYSQL_DSN not set — live MySQL applier test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	var conns []*sql.Conn
	for i := 0; i < 2; i++ { // held together: distinct connections
		c, err := a.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		conns = append(conns, c)
	}
	for i, c := range conns {
		var coll, mode string
		if err := c.QueryRowContext(ctx, `SELECT @@collation_connection, @@sql_mode`).Scan(&coll, &mode); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		if coll != "utf8mb4_0900_as_cs" || strings.Contains(mode, "NO_BACKSLASH_ESCAPES") {
			t.Errorf("connection %d: collation_connection=%q sql_mode=%q; want utf8mb4_0900_as_cs without NO_BACKSLASH_ESCAPES", i, coll, mode)
		}
	}
}
