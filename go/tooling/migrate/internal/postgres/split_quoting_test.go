package postgres

import (
	"reflect"
	"strings"
	"testing"
)

// A post-tx body is split on statement boundaries only. A `;` inside a
// quoted identifier, or after a backslash-escaped quote in an E” string, is
// text: splitting there tore the statement into two broken ones after the
// in-transaction half had already committed.
func TestSplitStatements_QuotedIdentifiersAndEscapeStrings(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"quoted identifier", `CREATE INDEX CONCURRENTLY "i;x" ON t (c); SELECT 1`,
			[]string{`CREATE INDEX CONCURRENTLY "i;x" ON t (c);`, "SELECT 1;"}},
		{"E-string escaped quote", `UPDATE t SET s = E'it\'s; fine'; SELECT 1`,
			[]string{`UPDATE t SET s = E'it\'s; fine';`, "SELECT 1;"}},
		{"plain string keeps backslash literal", `UPDATE t SET s = 'a\'; SELECT 1`,
			[]string{`UPDATE t SET s = 'a\';`, "SELECT 1;"}},
		{"positional parameter is not a dollar quote", "SELECT $1; SELECT $2",
			[]string{"SELECT $1;", "SELECT $2;"}},
		{"dollar body", "DO $$ BEGIN PERFORM 1; END $$; SELECT 1",
			[]string{"DO $$ BEGIN PERFORM 1; END $$;", "SELECT 1;"}},
		// A tag follows identifier rules, which admit a UTF-8 rune.
		{"non-ASCII dollar tag", "COMMENT ON TABLE t IS $é$a; b$é$; SELECT 1",
			[]string{"COMMENT ON TABLE t IS $é$a; b$é$;", "SELECT 1;"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitStatements(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitStatements(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Wipe drops public and every declared schema, each as an escaped
// identifier, and recreates an empty public — in one batch. public and
// repeats are folded; a system schema is refused.
func TestWipeSQL_DropsDeclaredSchemas(t *testing.T) {
	got, err := wipeSQL([]string{"billing", `we"ird`, "public", "billing", " "})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"DROP SCHEMA IF EXISTS public CASCADE;",
		`DROP SCHEMA IF EXISTS "billing" CASCADE;`,
		`DROP SCHEMA IF EXISTS "we""ird" CASCADE;`,
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("want exactly one %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"public"`) {
		t.Errorf("public dropped twice:\n%s", got)
	}
	if !strings.HasSuffix(got, "CREATE SCHEMA public;") {
		t.Errorf("public is not recreated last:\n%s", got)
	}
	for _, sys := range []string{"pg_catalog", "information_schema", "pg_toast"} {
		if _, err := wipeSQL([]string{sys}); err == nil {
			t.Errorf("system schema %s accepted", sys)
		}
	}
}
