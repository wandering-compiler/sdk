package fingerprint

// The observation reports what
// IDENTIFIES a primary key and a foreign key, not only their columns: the
// key constraint's name (Postgres names an inline key `<table>_pkey`, a
// hand-made one says anything), the schema a key's target lives in, and
// every column the key references. Measured against a real catalogue,
// because each of the three is a catalogue join that a mock would only
// restate.
//
// Gated on W17_PG_DSN like the other live lanes; W17_REQUIRE_LIVE_MIGRATOR=1
// turns an unwired run into a failure instead of a skip.

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestObservePostgres_KeyIdentity_Live(t *testing.T) {
	dsn := os.Getenv("W17_PG_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_PG_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_PG_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	name := fmt.Sprintf("w17_fp_keyid_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create db: %v", err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`) }()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	db, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	defer func() { _ = db.Close(ctx) }()

	for _, s := range []string{
		`CREATE SCHEMA other`,
		`CREATE TABLE other.u (x bigint, y bigint, CONSTRAINT u_xy UNIQUE (x, y))`,
		`CREATE TABLE t (a bigint, b bigint, CONSTRAINT my_pk PRIMARY KEY (a, b),
		   CONSTRAINT t_ab_fk FOREIGN KEY (a, b) REFERENCES other.u (x, y))`,
		`CREATE TABLE plain (id bigint PRIMARY KEY)`,
		`CREATE TABLE keyless (n int)`,
	} {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	obs, err := ObservePostgres(ctx, db)
	if err != nil {
		t.Fatalf("ObservePostgres: %v", err)
	}
	by := map[string]ObservedTable{}
	for _, tb := range obs.Tables {
		by[tb.Schema+"."+tb.Name] = tb
	}

	if got := by["public.t"].PrimaryKeyName; got != "my_pk" {
		t.Errorf("t.PrimaryKeyName = %q, want my_pk — a key change that drops the DERIVED t_pkey no-ops and its ADD PRIMARY KEY fails 42P16", got)
	}
	if got := by["public.plain"].PrimaryKeyName; got != "plain_pkey" {
		t.Errorf("plain.PrimaryKeyName = %q, want plain_pkey", got)
	}
	if got := by["public.keyless"].PrimaryKeyName; got != "" {
		t.Errorf("keyless.PrimaryKeyName = %q, want empty", got)
	}
	fks := by["public.t"].ForeignKeys
	if len(fks) != 1 {
		t.Fatalf("t.ForeignKeys = %+v, want one", fks)
	}
	fk := fks[0]
	if fk.TargetSchema != "other" {
		t.Errorf("TargetSchema = %q, want other — without it the DOWN re-add references the OWNER's schema", fk.TargetSchema)
	}
	if !reflect.DeepEqual(fk.Columns, []string{"a", "b"}) || !reflect.DeepEqual(fk.TargetColumns, []string{"x", "y"}) {
		t.Errorf("Columns/TargetColumns = %v / %v, want [a b] / [x y] — a composite key restated from its first column is a narrower constraint", fk.Columns, fk.TargetColumns)
	}
	if fk.TargetColumn != "x" {
		t.Errorf("TargetColumn = %q, want x (kept for old servers)", fk.TargetColumn)
	}
}
