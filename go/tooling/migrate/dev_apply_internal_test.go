package migrate

import (
	"strings"
	"testing"

	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
)

// TestDevApplySQL_FoldsAndStripsConcurrently — dev folds the post-tx
// body into the transactional SQL with CONCURRENTLY stripped (so the
// index build runs in-transaction, no wc_migrations phase machinery).
// The index itself is unchanged — only its build method — preserving
// the collapse-equivalence invariant.
func TestDevApplySQL_FoldsAndStripsConcurrently(t *testing.T) {
	m := &applyplanpb.DevMigration{
		UpSql:       "BEGIN;\nALTER TABLE users ADD COLUMN age INTEGER;\nCOMMIT;",
		UpSqlPostTx: "CREATE INDEX CONCURRENTLY users_age_idx ON users (age);",
	}
	got := devApplySQL(m)
	if !strings.Contains(got, "ALTER TABLE users ADD COLUMN age") {
		t.Errorf("dev SQL dropped the transactional body:\n%s", got)
	}
	if strings.Contains(strings.ToUpper(got), "CONCURRENTLY") {
		t.Errorf("CONCURRENTLY not stripped:\n%s", got)
	}
	if !strings.Contains(got, "CREATE INDEX users_age_idx ON users (age)") {
		t.Errorf("index build not folded in (de-concurrented):\n%s", got)
	}
}

// TestDevApplySQL_NoPostTx — a migration with no post-tx is its up_sql
// verbatim (no trailing newline appended).
func TestDevApplySQL_NoPostTx(t *testing.T) {
	m := &applyplanpb.DevMigration{UpSql: "CREATE TABLE t (id BIGINT);"}
	if got := devApplySQL(m); got != "CREATE TABLE t (id BIGINT);" {
		t.Errorf("devApplySQL = %q, want verbatim up_sql", got)
	}
}

// TestConcurrentlyRe_WholeWordOnly — the strip matches CONCURRENTLY as a
// keyword, never an identifier that merely contains the substring.
func TestConcurrentlyRe_WholeWordOnly(t *testing.T) {
	// A column/table named with the substring must survive.
	safe := "CREATE INDEX ix ON jobs (run_concurrently_flag);"
	if got := concurrentlyRe.ReplaceAllString(safe, ""); got != safe {
		t.Errorf("mangled an identifier containing the substring:\n got %q\nwant %q", got, safe)
	}
	// The real keyword (preceded by whitespace) is stripped.
	kw := "CREATE INDEX CONCURRENTLY ix ON jobs (x);"
	if got := concurrentlyRe.ReplaceAllString(kw, ""); strings.Contains(strings.ToUpper(got), "CONCURRENTLY") {
		t.Errorf("keyword not stripped: %q", got)
	}
}
