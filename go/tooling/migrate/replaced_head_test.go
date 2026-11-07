package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// A database that applied a migration the console later REPLACED
// (`w17ctl migrate replace`) sits at an id the new history does not contain.
// The replacement — planned from the schema BEFORE the replaced migration —
// carries a later id, so the id filter alone would run it on top of the
// migration it replaces. Apply must refuse and run nothing.
func TestPlan_RefusesAHeadTheHistoryReplaced(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE x (id BIGINT);")
	repl := mkMig("ts-3", "main", "ALTER TABLE x ADD COLUMN name TEXT;")
	dir := seedDir(t, m1, repl) // the live history: ts-1 → ts-3
	stubA := stub.New()
	stubA.Head = "ts-2" // applied the replaced migration

	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       targets(lockTarget{"main", repl.GetId(), repl.GetContentSha256()}),
		MigrationsDir: dir,
		ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
	})
	if err == nil {
		t.Fatal("a database at a replaced migration was migrated on — the replacement ran on top of the migration it replaces")
	}
	for _, want := range []string{"ts-2", "replace", "rollback --to ts-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not say %q:\n%v", want, err)
		}
	}
	if n := len(stubA.Calls()); n != 0 {
		t.Errorf("the applier ran %d migration(s) after the refusal", n)
	}
}

// The guard is narrow: a database whose head is ON the chain, AHEAD of the
// pinned target, or below the whole chain keeps the behaviour it had.
func TestPlan_HeadOffChainGuardLeavesOrdinaryHeadsAlone(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE x (id BIGINT);")
	m2 := mkMig("ts-2", "main", "ALTER TABLE x ADD COLUMN a TEXT;")
	dir := seedDir(t, m1, m2)
	for _, tc := range []struct {
		head    string
		pending int
	}{
		{"", 2},     // fresh
		{"ts-1", 1}, // on the chain
		{"ts-2", 0}, // at the target
		{"ts-9", 0}, // ahead of the lock (an older release redeployed)
	} {
		stubA := stub.New()
		stubA.Head = tc.head
		pending, err := migrate.Plan(context.Background(), migrate.Config{
			Targets:       targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()}),
			MigrationsDir: dir,
			ApplierFor:    func(_ string) (migrate.Applier, error) { return stubA, nil },
		})
		if err != nil {
			t.Errorf("head %q: %v", tc.head, err)
			continue
		}
		if len(pending) != tc.pending {
			t.Errorf("head %q: %d pending, want %d", tc.head, len(pending), tc.pending)
		}
	}
}
