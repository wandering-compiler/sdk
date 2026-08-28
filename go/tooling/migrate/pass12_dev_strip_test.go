package migrate

import (
	"strings"
	"testing"

	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
)

// T2-5 pass #12 (F11, A12-12, LOW) — the dev CONCURRENTLY strip was
// content-blind.
//
// `post_tx` is where authored raw bodies live, and "the author owns what they
// persist" is a standing owner decision — so a tool silently rewriting their
// text breaks it even on the dev path. The word boundary the old comment
// relied on says nothing about WHERE in the body the match sits.
func TestDevApplySQL_LeavesLiteralsAndCommentsAlone(t *testing.T) {
	post := "CREATE INDEX CONCURRENTLY idx ON t (c);\n" +
		"INSERT INTO audit (note) VALUES ('built CONCURRENTLY by hand');\n" +
		"-- rebuilt CONCURRENTLY on 2026-01-01\n" +
		"/* also CONCURRENTLY */\n"

	got := devApplySQL(&applyplanpb.DevMigration{UpSql: "BEGIN;", UpSqlPostTx: post})

	if strings.Contains(got, "INDEX CONCURRENTLY") {
		t.Error("the executable CONCURRENTLY was not stripped — dev needs the index build inside its transaction")
	}
	for _, want := range []string{
		"'built CONCURRENTLY by hand'",
		"-- rebuilt CONCURRENTLY on 2026-01-01",
		"/* also CONCURRENTLY */",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the strip rewrote author-owned text: %q is missing from\n%s", want, got)
		}
	}
}

// A doubled quote is an escaped quote INSIDE the literal, not its end. Getting
// that wrong would resume "executable" scanning mid-string and strip there.
func TestStripConcurrently_HandlesEscapedQuotes(t *testing.T) {
	in := "SELECT 'it''s CONCURRENTLY fine';\nCREATE INDEX CONCURRENTLY i ON t (c);"
	got := stripConcurrently(in)
	if !strings.Contains(got, "'it''s CONCURRENTLY fine'") {
		t.Errorf("a doubled quote ended the literal early: %s", got)
	}
	if strings.Contains(got, "INDEX CONCURRENTLY") {
		t.Errorf("the executable keyword survived: %s", got)
	}
}
