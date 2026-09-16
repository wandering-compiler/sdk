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
	// ⚠️ This assertion used to read "one string, so one batch, so one
	// transaction" and refuse any `BEGIN;` — and it passed because the
	// FIXTURE above carries no envelope. Production bodies do: the emitter's
	// `wrapTransaction` writes `BEGIN; … COMMIT;`, so the real batch always
	// contained one and the baseline was appended AFTER the COMMIT, in its
	// own autocommit. The test asserted a property of its own input rather
	// than of the code (T3-7 pass #14, D14-4).
	//
	// What the atomicity claim actually requires is checked in
	// `pass14_baseline_atomicity_test.go`, against the emitter's byte shape:
	// the baseline goes INSIDE the envelope when there is one.
	//
	// Here, with an envelope-free body, the only thing left to say is that
	// nothing invents one.
	if strings.Contains(got, "BEGIN;") {
		t.Fatalf("invented a transaction for a body that carries none:\n%s", got)
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
