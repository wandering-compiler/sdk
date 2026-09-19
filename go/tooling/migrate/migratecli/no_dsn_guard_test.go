package migratecli

import (
	"context"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

func TestFixturesApply_NoDSNAnywhereIsAnError(t *testing.T) {
	rec := &recordingSeeder{}
	withSeeder(t, rec)
	// Rows to seed, deliberately: an EMPTY group is an honest success and
	// takes the friendly path before this guard is reached. The failure only
	// makes sense when there is something to write and nowhere to write it.
	dir := seedDir(t, migrate.FixtureSeed{
		Domain: "core", Name: "a-rows",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO rows")},
	})
	lock := writeLock(t, migrate.LockConnection{Name: "core-postgres", TargetMigrationID: "ts-1"})
	var out strings.Builder

	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Getenv: envMap{}.get, Out: &out}, &out)
	if err == nil {
		t.Fatalf("a run with no DSN must fail; it printed:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "W17_TARGET_CORE_POSTGRES") {
		t.Errorf("the refusal does not name the missing variable: %v", err)
	}
	// openSeedTarget refuses this case too, as a STORE-TYPE problem. That
	// refusal is correct for a bundle serving only Redis and wrong for the
	// far commoner "nobody exported the variable" — and because it fires on
	// the same input, a test that only asserts "some error" passes whichever
	// one runs. Assert the CAUSE, or the earlier check can be deleted without
	// a single test noticing.
	if strings.Contains(err.Error(), "a store a fixture can be seeded into") {
		t.Errorf("a missing DSN was reported as a store-type problem, which sends the reader to the wrong fix: %v", err)
	}
}

// The fixtures counterpart: an empty group is an honest success, and stays one
// even with nothing exported.
func TestFixturesApply_EmptyGroupAndNoDSNIsStillFine(t *testing.T) {
	rec := &recordingApplier{}
	withApplier(t, rec)
	lock := writeLock(t, migrate.LockConnection{Name: "core-postgres"})
	var out strings.Builder

	if err := runFixtures(context.Background(),
		[]string{"apply", "--lock", lock, "--fixtures", t.TempDir(), "--group", "nosuchgroup"},
		Options{Getenv: envMap{}.get, Out: &out}, &out); err != nil {
		t.Fatalf("an empty fixture group must not demand a DSN: %v\n%s", err, out.String())
	}
}
