package fingerprint

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// T1-1 pass #49 B49-8 — the one-member arm (F8b) could not read Postgres's
// rendering of a VARCHAR column: the left side comes back CAST,
// `(((v1)::text = 'draft'::text))` (measured on postgres:14/16/18), not a
// bare identifier. The member set then read as "not a membership check", the
// declared check was carried, and an author's second choice never planned the
// ReplaceCheck — the database kept refusing it (23514) behind a converged
// sync.
func TestP49_B49_8_OneMemberOnVarcharIsAMembershipCheck(t *testing.T) {
	cases := []struct {
		def  string
		want []string
		ok   bool
	}{
		{def: "CHECK (((v1)::text = 'draft'::text))", want: []string{"draft"}, ok: true},
		{def: `CHECK ((("v1")::text = 'draft'::text))`, want: []string{"draft"}, ok: true},
		{def: "CHECK (((code)::character varying(3) = 'abc'::bpchar))", want: []string{"abc"}, ok: true},
		// The neighbours that must stay where they were.
		{def: "CHECK ((v2 = 'draft'::text))", want: []string{"draft"}, ok: true},
		{def: "CHECK ((n = 1))", want: []string{"1"}, ok: true},
		// A cast of something that is NOT a bare column is still not a
		// membership test — the unwrap must not widen the arm.
		{def: "CHECK (((lower(v1))::text = 'draft'::text))", ok: false},
		{def: "CHECK (((v1 || v2)::text = 'draft'::text))", ok: false},
		{def: "CHECK (((v1)::text = (v2)::text))", ok: false},
	}
	for _, c := range cases {
		got, ok := CheckMembersFromDef(c.def)
		if ok != c.ok || (ok && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("CheckMembersFromDef(%q) = %v, %v; want %v, %v", c.def, got, ok, c.want, c.ok)
		}
	}
}

// liveConn opens a FRESH database on the server W17_PG_DSN points at, so an
// observation reads exactly what the test created and nothing else.
func liveConn(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("W17_PG_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_PG_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_PG_DSN not set — live observation test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	db := fmt.Sprintf("fp_k5_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.Database = db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", db, err)
	}
	t.Cleanup(func() {
		_ = conn.Close(context.Background())
		a, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			_ = a.Close(context.Background())
		}
	})
	return conn
}

// T1-1 pass #49 A49-1 / A49-14 / D49-4 — the observation carries every
// native enum type with its labels in SORT order, spelled the way a column
// of it reports its type, so the console can tie the two together.
func TestP49_A49_1_ObservePostgresReportsEnumLabels(t *testing.T) {
	conn := liveConn(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE SCHEMA s1`,
		`CREATE TYPE s1.t_status AS ENUM ('a', 'b')`,
		`CREATE TYPE pub_status AS ENUM ('z', 'y')`,
		`ALTER TYPE pub_status ADD VALUE 'x' BEFORE 'z'`,
		`CREATE TABLE s1.t (id bigint PRIMARY KEY, status s1.t_status NOT NULL)`,
		`CREATE TABLE p (id bigint PRIMARY KEY, status pub_status)`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	obs, err := ObservePostgres(ctx, conn)
	if err != nil {
		t.Fatalf("ObservePostgres: %v", err)
	}
	byType := map[string]ObservedEnumType{}
	for _, et := range obs.EnumTypes {
		byType[et.Type] = et
	}
	colType := map[string]string{}
	for _, tb := range obs.Tables {
		for _, c := range tb.Columns {
			colType[tb.Schema+"."+tb.Name+"."+c.Name] = c.DataType
		}
	}
	want := map[string]ObservedEnumType{
		"s1.t.status":     {Schema: "s1", Name: "t_status", Labels: []string{"a", "b"}},
		"public.p.status": {Schema: "public", Name: "pub_status", Labels: []string{"x", "z", "y"}},
	}
	for col, w := range want {
		et, ok := byType[colType[col]]
		if !ok {
			t.Fatalf("column %s reports type %q, which no observed enum type spells (enum types: %+v)",
				col, colType[col], obs.EnumTypes)
		}
		if et.Schema != w.Schema || et.Name != w.Name || !reflect.DeepEqual(et.Labels, w.Labels) {
			t.Errorf("column %s: enum = %+v, want schema=%s name=%s labels=%v", col, et, w.Schema, w.Name, w.Labels)
		}
	}
}

// T1-1 pass #49 B49-10 — UNIQUE constraints are observed (name + columns in
// key order), and a table's UniquesRead says they were read even when it
// holds none; a unique INDEX (not a constraint) stays an index.
func TestP49_B49_10_ObservePostgresReportsUniques(t *testing.T) {
	conn := liveConn(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE notes (id bigint PRIMARY KEY, tenant_id bigint, UNIQUE (tenant_id, id))`,
		`CREATE TABLE plain (id bigint PRIMARY KEY, code text)`,
		`CREATE UNIQUE INDEX plain_code_idx ON plain (code)`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	obs, err := ObservePostgres(ctx, conn)
	if err != nil {
		t.Fatalf("ObservePostgres: %v", err)
	}
	for _, tb := range obs.Tables {
		if !tb.UniquesRead {
			t.Errorf("%s: UniquesRead = false — a reader that looked must say so", tb.Name)
		}
		switch tb.Name {
		case "notes":
			if len(tb.Uniques) != 1 || tb.Uniques[0].Name != "notes_tenant_id_id_key" ||
				!reflect.DeepEqual(tb.Uniques[0].Columns, []string{"tenant_id", "id"}) {
				t.Errorf("notes uniques = %+v", tb.Uniques)
			}
		case "plain":
			if len(tb.Uniques) != 0 {
				t.Errorf("a unique INDEX was reported as a constraint: %+v", tb.Uniques)
			}
		}
	}
}
