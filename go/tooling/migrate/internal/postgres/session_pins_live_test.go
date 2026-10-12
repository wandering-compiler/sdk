package postgres

import (
	"context"
	"os"
	"testing"
	"time"
)

// The applier's connection runs with the shared session pins, read back
// from the server: standard_conforming_strings on and search_path public
// (the server default is "$user", public).
func TestNew_ConnectionCarriesTheSessionPins_Live(t *testing.T) {
	dsn := os.Getenv("W17_PG_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_PG_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_PG_DSN not set — live Postgres applier test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	var scs, path string
	if err := a.conn.QueryRow(ctx, `SELECT current_setting('standard_conforming_strings'), current_setting('search_path')`).Scan(&scs, &path); err != nil {
		t.Fatal(err)
	}
	if scs != "on" || path != "public" {
		t.Errorf("standard_conforming_strings=%q search_path=%q; want on, public", scs, path)
	}
}
