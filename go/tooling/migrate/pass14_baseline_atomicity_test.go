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
