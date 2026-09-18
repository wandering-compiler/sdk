package migrate

import (
	"strings"
	"testing"

	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
)

// TestDevApplySQL_RecordsTheBaselineInsideTheSchemaTransaction — T3-7 pass
// #14, `D14-4`.
//
// Building a schema from scratch writes an applied-ledger BASELINE so the
// next `migrate apply` knows the database already holds everything up to
// that point. The two have to land together: a schema with no baseline is
// silently treated as "already at that state" by `storeHasSchema`, which
// green-skips it forever (D14-8), so the crash window between them is not
// a retry away from being fixed — it is permanent and silent.
//
// The code appended the baseline AFTER the schema body. The existing test
// asserted the batch carries no `BEGIN;` and passed, because it fed a
// fixture production never produces: the real `up_sql` comes from the
// emitter's `wrapTransaction`, which emits `BEGIN; … COMMIT;`. So the
// baseline was appended after an explicit COMMIT and ran in its own
// autocommit — exactly the window the pairing exists to remove. Measured
// live on pg18: the table survives a batch that errored on its post-COMMIT
// tail.
//
// The fix is to put the baseline INSIDE the envelope when there is one. A
// body with no envelope keeps the old shape, which is what a dialect
// without transactional DDL needs.
func TestDevApplySQL_RecordsTheBaselineInsideTheSchemaTransaction(t *testing.T) {
	m := &applyplanpb.DevMigration{
		// The byte shape `emit.wrapTransaction` produces.
		UpSql:       "BEGIN;\n\nCREATE TABLE t (id INT PRIMARY KEY);\n\nCOMMIT;\n",
		BaselineSql: "INSERT INTO w17_migrations (timestamp) VALUES ('20260101T000000Z');",
	}

	got := devApplySQL(m)

	commit := strings.LastIndex(got, "COMMIT;")
	baseline := strings.Index(got, "INSERT INTO w17_migrations")
	if commit < 0 || baseline < 0 {
		t.Fatalf("batch is missing one of its halves:\n%s", got)
	}
	if baseline > commit {
		t.Errorf("the baseline is written AFTER the schema transaction commits.\n\n%s\n\n"+
			"A crash between the COMMIT and the baseline leaves a built schema with an empty "+
			"ledger — and `storeHasSchema` then green-skips that database forever, so it is "+
			"not a state a retry recovers from (T3-7 pass #14, D14-4).", got)
	}
}

// A body with no transaction envelope keeps the appended shape: dialects
// without transactional DDL have nothing to be inside of, and inventing a
// BEGIN for them would be worse than the window.
func TestDevApplySQL_LeavesAnUnwrappedBodyAlone(t *testing.T) {
	m := &applyplanpb.DevMigration{
		UpSql:       "CREATE TABLE t (id INT PRIMARY KEY);",
		BaselineSql: "INSERT INTO w17_migrations (timestamp) VALUES ('x');",
	}
	got := devApplySQL(m)
	if strings.Contains(got, "BEGIN;") {
		t.Errorf("invented a transaction for a body that carries none:\n%s", got)
	}
	if !strings.Contains(got, "INSERT INTO w17_migrations") {
		t.Errorf("the baseline vanished:\n%s", got)
	}
}

// TestDevApplySQL_PostTxAndBaselineShareTheEnvelope — T3-7 pass #15,
// `C15-9` / `B15-5`. Two defects in pass #14's fix, both measured by a
// verifier against the real function.
//
// **The post-tx half landed outside.** Splicing only the baseline before the
// final COMMIT put the ledger row INSIDE the transaction and left the
// post-tx statements after it — so a crash between them commits "this
// schema is built" with the tail missing. That is the same tear D14-4 closed,
// pointing the other way, produced by the fix for it.
//
// In THIS path the post-tx body is safe to run inside a transaction: it has
// already been through `stripConcurrently`, which is what makes a
// CONCURRENTLY index legal in one. So everything belongs in one envelope.
//
// **The splice point was a substring search.** `LastIndex(body, "COMMIT;")`
// matches a COMMIT in an author comment or a string literal — measured: a
// post-tx statement containing `'after COMMIT; rebuild stats'` had the
// baseline spliced into the middle of the literal. The repo already has the
// correct scanner (`stripConcurrently` walks comments and quoted strings);
// it just was not used.
func TestDevApplySQL_PostTxAndBaselineShareTheEnvelope(t *testing.T) {
	m := &applyplanpb.DevMigration{
		UpSql:       "BEGIN;\n\nCREATE TABLE t (id INT PRIMARY KEY);\n\nCOMMIT;\n",
		UpSqlPostTx: "CREATE INDEX CONCURRENTLY i ON t (id);",
		BaselineSql: "INSERT INTO w17_migrations (timestamp) VALUES ('20260101T000000Z');",
	}
	got := devApplySQL(m)

	commit := strings.LastIndex(got, "COMMIT;")
	index := strings.Index(got, "CREATE INDEX")
	baseline := strings.Index(got, "INSERT INTO w17_migrations")
	if commit < 0 || index < 0 || baseline < 0 {
		t.Fatalf("batch is missing a half:\n%s", got)
	}
	if index > commit {
		t.Errorf("the post-tx statements run AFTER the transaction commits, while the "+
			"baseline runs inside it — so a crash between them leaves the ledger saying "+
			"the schema is built with its tail missing:\n\n%s", got)
	}
	if baseline > commit {
		t.Errorf("the baseline is written after the commit:\n\n%s", got)
	}
}

// The splice must not land inside author text that merely CONTAINS the word.
func TestDevApplySQL_IgnoresACommitInsideALiteral(t *testing.T) {
	// The trap lives in `up_sql`, because that is the field an author can
	// reach: the raw-SQL escape hatch writes migration bodies, and a body
	// may legitimately contain the word in a comment or a string. Post-tx
	// now goes INSIDE the envelope, so it is no longer part of what gets
	// searched — which is why this case had to move here to stay real.
	m := &applyplanpb.DevMigration{
		// The author's mention comes AFTER the real commit — a trailing
		// comment, which is the ordinary place for one. A substring search
		// finds THAT and splices the ledger row into the comment, where it
		// never executes: the schema commits and the baseline silently does
		// not, which is D14-4's exact state reached through the fix for it.
		UpSql: "BEGIN;\n\nCREATE TABLE t (id INT PRIMARY KEY);\n\nCOMMIT;\n" +
			"-- rerun this file if the COMMIT; above failed\n",
		BaselineSql: "INSERT INTO w17_migrations (timestamp) VALUES ('20260101T000000Z');",
	}
	got := devApplySQL(m)

	// The author's comment must survive byte-intact.
	if !strings.Contains(got, "-- rerun this file if the COMMIT; above failed") {
		t.Errorf("the splice cut the author's trailing comment in half:\n\n%s", got)
	}
	// And the baseline must land before the REAL commit — the one that ends
	// the transaction, not the one somebody wrote about.
	realCommit := strings.Index(got, "COMMIT;")
	b := strings.Index(got, "INSERT INTO w17_migrations")
	if b < 0 {
		t.Fatalf("the baseline vanished:\n\n%s", got)
	}
	if b > realCommit {
		t.Errorf("the baseline landed after the transaction committed — a substring search "+
			"found the word in the trailing comment and spliced there, so the ledger row "+
			"never executes and the schema commits without it:\n\n%s", got)
	}
}
