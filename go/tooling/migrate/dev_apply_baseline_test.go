package migrate

import (
	"strings"
	"testing"

	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
)

// The baseline has to reach the executed SQL, and it has to be LAST: it
// records that the schema above it landed, so a body that ran the record
// first would claim a migration for tables that had not been created yet.
func TestDevApplySQLAppendsBaselineLast(t *testing.T) {
	m := &applyplanpb.DevMigration{
		UpSql:       "CREATE TABLE t (id INT);",
		UpSqlPostTx: "CREATE INDEX CONCURRENTLY i ON t (id);",
		BaselineSql: "INSERT INTO w17_migrations VALUES ('20260910T120000Z');",
	}
	got := devApplySQL(m)

	for _, want := range []string{"CREATE TABLE t", "CREATE INDEX", "w17_migrations"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "CREATE TABLE t"), strings.Index(got, "w17_migrations"); i > j {
		t.Fatalf("ledger row precedes the schema it records:\n%s", got)
	}
	if strings.Contains(got, "CONCURRENTLY") {
		t.Fatalf("post-tx was folded in without stripping CONCURRENTLY:\n%s", got)
	}
	// One string, so one batch, so one transaction. A caller that split these
	// could leave the schema built and the ledger empty — a state nothing
	// recovers from, because the next run finds the store populated and skips.
	if strings.Count(got, "BEGIN;") > 0 {
		t.Fatalf("dev body opened its own transaction; it is executed as one implicit batch:\n%s", got)
	}
}

// An artefact from a project that has never been pushed carries no baseline.
// That must apply the schema and record nothing — not fail, and not leave a
// stray separator that changes the bytes an emitter test compares.
func TestDevApplySQLWithoutBaselineIsUnchanged(t *testing.T) {
	m := &applyplanpb.DevMigration{UpSql: "CREATE TABLE t (id INT);"}
	if got, want := devApplySQL(m), "CREATE TABLE t (id INT);"; got != want {
		t.Fatalf("body changed when no baseline is pinned\n got: %q\nwant: %q", got, want)
	}
}

// A baseline with no schema body is legitimate: the connection's tables were
// unchanged but the ledger still has to name where it stands.
func TestDevApplySQLBaselineOnly(t *testing.T) {
	m := &applyplanpb.DevMigration{BaselineSql: "INSERT INTO w17_migrations VALUES ('x');"}
	if got, want := devApplySQL(m), "INSERT INTO w17_migrations VALUES ('x');"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
