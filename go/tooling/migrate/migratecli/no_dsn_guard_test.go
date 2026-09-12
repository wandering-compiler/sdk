package migratecli

import (
	"context"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// A run with NO DSN anywhere must fail, not print a note and succeed.
//
// This is the shape deinvo reported (2026-09-12): both steps exited 0 having
// done nothing, a dependent compose service started behind
// `condition: service_completed_successfully`, and the green run was taken as
// proof the new path worked. The database was still being built by the old
// initdb.
func TestSchemaApply_NoDSNAnywhereIsAnError(t *testing.T) {
	rec := &recordingApplier{}
	withApplier(t, rec)
	dir := planDir(t, "core-postgres")
	lock := writeLock(t, migrate.LockConnection{Name: "core-postgres"})
	var out strings.Builder

	err := runSchema(context.Background(), []string{"apply", "--lock", lock, "--schema", dir},
		Options{Getenv: envMap{}.get, Out: &out}, &out)
	if err == nil {
		t.Fatalf("a run with no DSN must fail; it printed:\n%s", out.String())
	}
	// The refusal has to name the VARIABLE, because the thing that went wrong
	// is one the reader has to set, and a message about "connections" sends
	// them to the lock instead.
	if !strings.Contains(err.Error(), "W17_TARGET_CORE_POSTGRES") {
		t.Errorf("the refusal does not name the missing variable: %v", err)
	}
	if len(rec.applied) != 0 {
		t.Errorf("a refused run applied DDL anyway: %v", rec.applied)
	}
}

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

// --allow-no-dsn opts back in to the old behaviour. "Run this step only where
// it applies" is a real need; it just has to be said out loud.
func TestSchemaApply_AllowNoDSNSucceedsAndAppliesNothing(t *testing.T) {
	rec := &recordingApplier{}
	withApplier(t, rec)
	dir := planDir(t, "core-postgres")
	lock := writeLock(t, migrate.LockConnection{Name: "core-postgres"})
	var out strings.Builder

	if err := runSchema(context.Background(),
		[]string{"apply", "--lock", lock, "--schema", dir, "--allow-no-dsn"},
		Options{Getenv: envMap{}.get, Out: &out}, &out); err != nil {
		t.Fatalf("--allow-no-dsn must succeed: %v", err)
	}
	if len(rec.applied) != 0 {
		t.Errorf("--allow-no-dsn applied DDL: %v", rec.applied)
	}
}

// SOME, not every: a project with several connections may point one run at
// one database. Only "nothing set at all" is the shape of a forgotten export,
// and failing on a partial set would break a legitimate workflow.
func TestSchemaApply_PartialDSNStillRuns(t *testing.T) {
	rec := &recordingApplier{}
	withApplier(t, rec)
	dir := planDir(t, "core-postgres", "billing-postgres")
	lock := writeLock(t,
		migrate.LockConnection{Name: "core-postgres"},
		migrate.LockConnection{Name: "billing-postgres"})
	var out strings.Builder

	if err := runSchema(context.Background(), []string{"apply", "--lock", lock, "--schema", dir},
		Options{Getenv: envMap{"W17_TARGET_CORE_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out); err != nil {
		t.Fatalf("a run with one DSN set must proceed: %v\n%s", err, out.String())
	}
	if len(rec.applied) != 1 {
		t.Fatalf("applied %d plan(s), want the one with a DSN: %v", len(rec.applied), rec.applied)
	}
	if !strings.Contains(rec.applied[0], "core_postgres_t") {
		t.Errorf("applied the wrong connection's DDL: %q", rec.applied[0])
	}
}

// No work AND no DSN stays a success. The guard is about "nowhere to do it",
// not "nothing to do": a bundle whose connections carry no schema has nothing
// to apply, and demanding a DSN to prove that would fail a run that is
// legitimately empty. This is the distinction the old code could not draw —
// it reported both as the friendly note — and the reason the check reads the
// PLAN before it reads the environment.
func TestSchemaApply_NoWorkAndNoDSNIsStillFine(t *testing.T) {
	rec := &recordingApplier{}
	withApplier(t, rec)
	// The plan carries another bundle's connection only.
	dir := planDir(t, "billing-postgres")
	lock := writeLock(t,
		migrate.LockConnection{Name: "core-postgres"},
		migrate.LockConnection{Name: "billing-postgres"})
	var out strings.Builder

	if err := runSchema(context.Background(), []string{"apply", "--lock", lock, "--schema", dir},
		Options{Connections: []string{"core-postgres"}, Getenv: envMap{}.get, Out: &out}, &out); err != nil {
		t.Fatalf("a bundle with nothing in the plan must not demand a DSN: %v\n%s", err, out.String())
	}
	if len(rec.applied) != 0 {
		t.Errorf("applied something for a bundle with no work: %v", rec.applied)
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
