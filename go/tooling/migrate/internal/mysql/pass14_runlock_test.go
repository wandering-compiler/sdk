package mysql

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// TestApplier_ImplementsRunLockCapable — T3-7 pass #14, `D14-1`.
//
// `lock.go` says transactional SQL dialects need no run-lock because "their
// up_sql runs in a transaction whose w17_migrations INSERT has the migration
// id as a primary key, so a second concurrent run fails loudly on the unique
// violation instead of double-applying".
//
// That is true of Postgres and SQLite. It is FALSE of MySQL, and the
// verifier measured it on both mysql80 and mysql84 through the production
// applier: a body containing DDL implicit-commits, which ends the
// transaction mid-body and drops everything after it into autocommit. So
// two racing runs both apply the tail durably, and only THEN does the
// loser's ledger INSERT fail with ER 1062. The failure is loud and the
// damage is already committed — v=40 survived in the measurement, while a
// no-DDL control rolled back and the pg18 control held.
//
// The exposed population is narrower than "every migration", and the
// verifier named it: a generated body dies at its first non-idempotent DDL
// before reaching any DML. The corrupting shape is the raw-SQL escape
// hatch's `IF NOT EXISTS` DDL followed by data statements — which is
// precisely what the escape hatch is for.
//
// A compile-time assertion rather than a race harness. The race needs two
// processes against a live engine and would be a flake in unit scope; what
// must not regress is that the MySQL applier declares itself lockable at
// all, because the orchestrator type-asserts this interface and silently
// takes the unlocked path for anything that does not implement it.
func TestApplier_ImplementsRunLockCapable(t *testing.T) {
	var a any = (*Applier)(nil)
	if _, ok := a.(migrate.RunLockCapable); !ok {
		t.Fatal("the MySQL applier does not implement RunLockCapable, so the orchestrator " +
			"applies without a run lock.\n\n" +
			"The transactional-guarantee argument in lock.go does not hold here: DDL " +
			"implicit-commits, so a body's tail runs in autocommit and two racing runs " +
			"both apply it durably before the ledger INSERT rejects the loser. Measured " +
			"on mysql80 and mysql84 (T3-7 pass #14, D14-1).")
	}
}
