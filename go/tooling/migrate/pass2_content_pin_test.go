package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// T2-5 pass #2 T25-D2-1: the content-integrity gate degraded OPEN on a
// degenerate lock. `Plan` skips the tamper check when target_content_sha256 is
// empty (`want != "" && want != found`), so a lock whose pin was blanked — a
// hand-edit, or a lock predating the pin — applies a MIGRATION WITHOUT verifying
// its content against the pin. ContentHash always yields a non-empty value, so an
// empty pin on a set target is always an anomaly; treating it as "skip" rather
// than "refuse" fails open on the one control the offline client (which does not
// re-verify the signature — D4) has to catch a tampered fetched artifact.

// Apply: a pinned target with an EMPTY content hash must be REFUSED, not applied
// unchecked. (Proven exploit: with an empty pin, a tampered artifact sailed
// through; here we assert the guard.)
func TestPlan_PinnedTargetEmptyHash_Refused(t *testing.T) {
	m := mkMig("ts-1", "main", "real body")
	dir := seedDir(t, m)
	tg := targets(lockTarget{"main", "ts-1", ""}) // target set, hash blanked

	_, err := migrate.Plan(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir,
		ApplierFor: func(_ string) (migrate.Applier, error) { return stub.New(), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "target_content_sha256") {
		t.Errorf("a pinned target with an empty content hash must be refused (fail closed); got %v", err)
	}
}

// Rollback is destructive and must be authenticated the same way — a pinned
// target with an empty hash must be refused, not silently skip the pin check.
func TestPlanRollback_PinnedTargetEmptyHash_Refused(t *testing.T) {
	m1 := mkMig("ts-1", "main", "v0")
	m2 := mkMig("ts-2", "main", "v1")
	dir := seedDir(t, m1, m2)
	tg := targets(lockTarget{"main", "ts-2", ""}) // pinned target, hash blanked
	stubA := stub.New()
	stubA.Head = "ts-2"

	_, err := migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "ts-1",
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "target_content_sha256") {
		t.Errorf("rollback with a pinned target and empty hash must be refused; got %v", err)
	}
}

// Non-regression: rollback with NO pinned target (TargetMigrationID empty) is
// NORMAL — the target comes from ToMigrationID, not the lock pin — and must NOT
// be refused by the new guard.
func TestPlanRollback_NoPinnedTarget_StillWorks(t *testing.T) {
	m1 := mkMig("ts-1", "main", "v0")
	m2 := mkMig("ts-2", "main", "v1")
	dir := seedDir(t, m1, m2)
	tg := []migrate.ConnTarget{{Connection: "main"}} // no pin — legitimate for rollback
	stubA := stub.New()
	stubA.Head = "ts-2"

	if _, err := migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "ts-1",
		ApplierFor: func(_ string) (migrate.Applier, error) { return stubA, nil },
	}); err != nil {
		t.Fatalf("unpinned rollback must still work: %v", err)
	}
}
