package migrate

import (
	"strings"
	"testing"

	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
)

// Schema and extension names come from options an author wrote. Each is
// rendered as one quoted identifier, its embedded quotes doubled, so a quote
// in a name cannot end the identifier and turn the rest into statements.
func TestDevApplySQL_QuotesSchemaAndExtensionNames(t *testing.T) {
	got := devApplySQL(&applyplanpb.DevMigration{
		RequiredSchemas:    []string{`bill"ing`},
		RequiredExtensions: []string{`ext"; DROP TABLE t; --`},
		UpSql:              "CREATE TABLE t (id INT);",
	})
	for _, want := range []string{
		`CREATE SCHEMA IF NOT EXISTS "bill""ing";`,
		`CREATE EXTENSION IF NOT EXISTS "ext""; DROP TABLE t; --";`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
}

// Every identifier helper in the package escapes the same way.
func TestPGQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		"billing": `"billing"`,
		`a"b`:     `"a""b"`,
		`""`:      `""""""`,
	} {
		if got := pgQuoteIdent(in); got != want {
			t.Errorf("pgQuoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

// A `COMMIT;` inside a dollar-quoted body (a procedure that commits) is the
// author's text, not the envelope: the baseline lands before the REAL
// commit, and the body is left byte-for-byte as written.
func TestDevApplySQL_IgnoresACommitInsideADollarQuotedBody(t *testing.T) {
	proc := "CREATE PROCEDURE p() LANGUAGE plpgsql AS $body$\nBEGIN\n  UPDATE t SET n = 1;\n  COMMIT;\nEND\n$body$;"
	m := &applyplanpb.DevMigration{
		UpSql:       "BEGIN;\nCREATE TABLE t (n INT);\nCOMMIT;\n" + proc + "\n",
		BaselineSql: "INSERT INTO w17_migrations (timestamp) VALUES ('20260101T000000Z');",
	}
	got := devApplySQL(m)
	if !strings.Contains(got, proc) {
		t.Fatalf("the procedure body was altered:\n%s", got)
	}
	base := strings.Index(got, "INSERT INTO w17_migrations")
	envelope := strings.Index(got, "\nCOMMIT;\n")
	if base < 0 || envelope < 0 || base > envelope {
		t.Fatalf("baseline not spliced before the envelope's COMMIT:\n%s", got)
	}
}

// The envelope's COMMIT is recognised in any case; a lower-case one used to
// be missed, which put the baseline AFTER the transaction it belongs in.
func TestDevApplySQL_RecognisesCommitInAnyCase(t *testing.T) {
	for _, commit := range []string{"commit;", "Commit ;", "COMMIT WORK;", "commit transaction;"} {
		m := &applyplanpb.DevMigration{
			UpSql:       "begin;\ncreate table t (n int);\n" + commit + "\n",
			BaselineSql: "INSERT INTO w17_migrations (timestamp) VALUES ('20260101T000000Z');",
		}
		got := devApplySQL(m)
		base := strings.Index(got, "INSERT INTO w17_migrations")
		c := strings.Index(got, commit)
		if base < 0 || c < 0 || base > c {
			t.Errorf("%q: baseline not inside the transaction:\n%s", commit, got)
		}
	}
}

// CONCURRENTLY inside a dollar-quoted body (a function that builds DDL) is
// text, not the keyword, and survives the strip.
func TestStripConcurrently_LeavesDollarQuotedBodiesAlone(t *testing.T) {
	in := "CREATE INDEX CONCURRENTLY i ON t (n);\n" +
		"CREATE FUNCTION f() RETURNS text LANGUAGE sql AS $$ SELECT 'x' || ' CONCURRENTLY ' $$;\n" +
		"CREATE FUNCTION g() RETURNS text LANGUAGE sql AS $fn$ SELECT $q$ build CONCURRENTLY $q$ $fn$;"
	got := stripConcurrently(in)
	if !strings.HasPrefix(got, "CREATE INDEX i ON t (n);") {
		t.Errorf("the keyword was not stripped:\n%s", got)
	}
	for _, body := range []string{
		"$$ SELECT 'x' || ' CONCURRENTLY ' $$",
		"$fn$ SELECT $q$ build CONCURRENTLY $q$ $fn$",
	} {
		if !strings.Contains(got, body) {
			t.Errorf("dollar-quoted body altered; want %q in:\n%s", body, got)
		}
	}
}

// The scanner's other opaque forms: an E” string's backslash-escaped quote,
// a nested block comment, a quoted identifier, and a positional parameter
// that is not a dollar quote.
func TestScanSQL_OpaqueForms(t *testing.T) {
	cases := []struct {
		name, in string
		wantExec []string
	}{
		{"E-string backslash quote", `SELECT E'it\'s COMMIT;'; COMMIT;`, []string{"SELECT E", "; COMMIT;"}},
		{"nested block comment", "/* a /* COMMIT; */ b */ COMMIT;", []string{" COMMIT;"}},
		{"quoted identifier", `SELECT "COMMIT;" FROM t;`, []string{"SELECT ", " FROM t;"}},
		{"positional parameter", "SELECT $1, $2;", []string{"SELECT $1, $2;"}},
		{"dollar in identifier", "SELECT a$b$ FROM t;", []string{"SELECT a$b$ FROM t;"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exec []string
			var whole strings.Builder
			scanSQL(tc.in, func(s, e int, executable bool) {
				whole.WriteString(tc.in[s:e])
				if executable {
					exec = append(exec, tc.in[s:e])
				}
			})
			if whole.String() != tc.in {
				t.Fatalf("spans do not cover the input: %q", whole.String())
			}
			if strings.Join(exec, "|") != strings.Join(tc.wantExec, "|") {
				t.Errorf("executable spans = %q, want %q", exec, tc.wantExec)
			}
		})
	}
}

// A name carrying the preflight block's dollar tag would close the block
// early; the tag is chosen so no name contains it.
func TestExtensionPreflightSQL_TagAvoidsTheNames(t *testing.T) {
	got := extensionPreflightSQL([]string{"x$w17_ext$y"})
	if strings.HasPrefix(got, "DO $w17_ext$") {
		t.Fatalf("the block uses a tag a name contains:\n%s", got)
	}
	open := got[len("DO "):strings.Index(got, "\n")]
	if !strings.HasSuffix(got, "END\n"+open+";") {
		t.Fatalf("block opened with %s does not close with it:\n%s", open, got)
	}
}
