package migrate

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestRequiredSchemasFromManifest(t *testing.T) {
	got := requiredSchemasFromManifest(`{"required_extensions":["vector"],"required_schemas":["billing","audit"]}`)
	if len(got) != 2 || got[0] != "billing" || got[1] != "audit" {
		t.Fatalf("got %v", got)
	}
	for _, raw := range []string{"", "nope", `{"required_schemas":"billing"}`} {
		if got := requiredSchemasFromManifest(raw); len(got) != 0 {
			t.Errorf("manifest %q yielded %v, want nothing", raw, got)
		}
	}
}

func TestSchemaPrepSQL_QuotesEachName(t *testing.T) {
	sql := schemaPrepSQL([]string{"billing", `we"ird`})
	for _, want := range []string{`CREATE SCHEMA IF NOT EXISTS "billing";`, `CREATE SCHEMA IF NOT EXISTS "we""ird";`} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in\n%s", want, sql)
		}
	}
}

// One statement per Postgres connection, every schema its pending migrations
// need, deduped — and nothing at all for a non-Postgres connection or a run
// whose manifests declare none.
func TestPrepareSchemas_PerConnection(t *testing.T) {
	pg := &extFake{}
	other := &extFake{notPG: true}
	ac := newRunApplierCache(func(conn string) (Applier, error) {
		if conn == "kv" {
			return other, nil
		}
		return pg, nil
	}, io.Discard)
	pending := []Pending{
		{Connection: "billing-postgres", Migration: withManifest("m1", "billing-postgres", `{"required_schemas":["billing"]}`)},
		{Connection: "billing-postgres", Migration: withManifest("m2", "billing-postgres", `{"required_schemas":["billing","ledger"]}`)},
		{Connection: "kv", Migration: withManifest("k1", "kv", `{"required_schemas":["billing"]}`)},
	}
	if err := prepareSchemas(context.Background(), ac, pending); err != nil {
		t.Fatal(err)
	}
	if len(pg.ran) != 1 || pg.ran[0] != "CREATE SCHEMA IF NOT EXISTS \"billing\";\nCREATE SCHEMA IF NOT EXISTS \"ledger\";\n" {
		t.Errorf("postgres ran %q", pg.ran)
	}
	if len(other.ran) != 0 {
		t.Errorf("a non-Postgres connection was sent CREATE SCHEMA: %q", other.ran)
	}

	none := &extFake{}
	ac = newRunApplierCache(func(string) (Applier, error) { return none, nil }, io.Discard)
	if err := prepareSchemas(context.Background(), ac, []Pending{{Connection: "main", Migration: withManifest("m", "main", "")}}); err != nil || len(none.ran) != 0 {
		t.Errorf("no declared schema: err=%v ran=%q", err, none.ran)
	}
}
