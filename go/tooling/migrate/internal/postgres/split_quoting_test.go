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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitStatements(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitStatements(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Wipe drops public and every owned schema, each as an escaped identifier,
// and recreates an empty public — in one batch.
func TestWipeSQL_DropsEveryOwnedSchema(t *testing.T) {
	got := wipeSQL([]string{"billing", `we"ird`})
	for _, want := range []string{
		"DROP SCHEMA IF EXISTS public CASCADE;",
		`DROP SCHEMA IF EXISTS "billing" CASCADE;`,
		`DROP SCHEMA IF EXISTS "we""ird" CASCADE;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "CREATE SCHEMA public;") {
		t.Errorf("public is not recreated last:\n%s", got)
	}
}
