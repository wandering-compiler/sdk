package migratecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// recordingSeeder is a store that remembers what one seeding run handed it.
type recordingSeeder struct {
	calls [][]migrate.SeedStmt
}

func (r *recordingSeeder) ExecSeed(_ context.Context, stmts []migrate.SeedStmt) error {
	r.calls = append(r.calls, stmts)
	return nil
}
func (r *recordingSeeder) Close() error { return nil }
func (r *recordingSeeder) Apply(context.Context, *applyfetchpb.Migration) error {
	panic("not used")
}
func (r *recordingSeeder) AppliedHead(context.Context) (string, error) { panic("not used") }
func (r *recordingSeeder) Rollback(context.Context, *applyfetchpb.Migration) error {
	panic("not used")
}

// kvOnlyApplier is a store with no seed support — a Redis, in effect.
type kvOnlyApplier struct{}

func (kvOnlyApplier) Close() error                                         { return nil }
func (kvOnlyApplier) Apply(context.Context, *applyfetchpb.Migration) error { panic("not used") }
func (kvOnlyApplier) AppliedHead(context.Context) (string, error)          { panic("not used") }
func (kvOnlyApplier) Rollback(context.Context, *applyfetchpb.Migration) error {
	panic("not used")
}

// seedDir writes rendered seeds to a temp dir and returns it.
func seedDir(t *testing.T, seeds ...migrate.FixtureSeed) string {
	t.Helper()
	root := t.TempDir()
	for _, s := range seeds {
		if err := migrate.WriteFixtureSeed(root, s); err != nil {
			t.Fatalf("WriteFixtureSeed %s/%s: %v", s.Domain, s.Name, err)
		}
	}
	return root
}

func seedStmt(sql string) *applyfetchpb.SeedStmt {
	return &applyfetchpb.SeedStmt{Sql: sql, Args: []*structpb.Value{structpb.NewStringValue("x")}}
}

// withSeeder swaps the applier factory for the run, and hands back what the
// store was asked to do.
func withSeeder(t *testing.T, ap migrate.Applier) {
	t.Helper()
	prev := newApplierFor
	newApplierFor = func([]factory.TargetSpec) migrate.ApplierFor {
		return func(string) (migrate.Applier, error) { return ap, nil }
	}
	t.Cleanup(func() { newApplierFor = prev })
}

// The whole group is ONE unit of work.
//
// Fixtures reference each other across files — a tasks fixture FKs the
// categories a taxonomy one creates — and the renderer orders rows WITHIN a
// file, not between them. Handing the store one file at a time would let a
// failure in the third leave the first two behind: a database in a state no
// fixture describes, which is worse than an unseeded one because what is there
// looks deliberate.
//
// So the assertion is that the store is called ONCE with everything, not that
// every statement arrived. A per-file implementation passes any check that
// only counts statements.
func TestFixturesApply_TheWholeGroupIsOneUnitOfWork(t *testing.T) {
	rec := &recordingSeeder{}
	withSeeder(t, rec)
	dir := seedDir(t,
		migrate.FixtureSeed{Domain: "app", Name: "a-categories", Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO categories")}},
		migrate.FixtureSeed{Domain: "app", Name: "b-tasks", Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO tasks")}},
	)
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres", TargetMigrationID: "ts-1"})
	var out strings.Builder
	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out)
	if err != nil {
		t.Fatalf("runFixtures: %v\n%s", err, out.String())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("store called %d time(s); the group must arrive as one transaction", len(rec.calls))
	}
	if len(rec.calls[0]) != 2 {
		t.Fatalf("got %d statement(s) in the one call, want 2", len(rec.calls[0]))
	}
	// FK order is the file order, and the file order is (domain, name).
	if !strings.Contains(rec.calls[0][0].SQL, "categories") {
		t.Errorf("statements are out of order: %+v", rec.calls[0])
	}
}

// A named group's rows do not arrive on a plain run. Groups are how a project
// separates dev data from production data, so a default run picking up `demo`
// would put showcase rows in whatever database it was pointed at.
func TestFixturesApply_DefaultRunLeavesNamedGroupsAlone(t *testing.T) {
	rec := &recordingSeeder{}
	withSeeder(t, rec)
	dir := seedDir(t,
		migrate.FixtureSeed{Domain: "app", Name: "base", Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}},
		migrate.FixtureSeed{Domain: "app", Name: "demo/showcase", Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO showcase")}},
	)
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres", TargetMigrationID: "ts-1"})
	var out strings.Builder
	if err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out); err != nil {
		t.Fatalf("runFixtures: %v", err)
	}
	if len(rec.calls) != 1 || len(rec.calls[0]) != 1 {
		t.Fatalf("want exactly the default group's one statement, got %+v", rec.calls)
	}
	if strings.Contains(rec.calls[0][0].SQL, "showcase") {
		t.Error("a named group was applied by a run that did not ask for it")
	}
}

// `--group .` is the natural way to write "no group" and reads as such. It
// once produced the registry key `./acl-roles`, matched nothing, and surfaced
// as a NotFound naming a key nobody wrote (deinvo, 2026-09-04).
func TestFixturesApply_DotMeansTheDefaultGroup(t *testing.T) {
	rec := &recordingSeeder{}
	withSeeder(t, rec)
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "base",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}})
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres", TargetMigrationID: "ts-1"})
	var out strings.Builder
	if err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir, "--group", "."},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out); err != nil {
		t.Fatalf("runFixtures: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("`--group .` seeded nothing; it must mean the default group. output:\n%s", out.String())
	}
}

// A bundle serving two databases has no defensible default. Picking the first
// would put a project's rows in whichever connection happened to sort earlier,
// and it would look like it worked.
func TestFixturesApply_TwoSeedableConnectionsMustBeDisambiguated(t *testing.T) {
	withSeeder(t, &recordingSeeder{})
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "base",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}})
	lock := writeLock(t,
		migrate.LockConnection{Name: "app-postgres", TargetMigrationID: "ts-1"},
		migrate.LockConnection{Name: "billing-postgres", TargetMigrationID: "ts-2"})
	var out strings.Builder
	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Getenv: envMap{
			"W17_TARGET_APP_POSTGRES":     "postgres://x/app",
			"W17_TARGET_BILLING_POSTGRES": "postgres://x/billing",
		}.get, Out: &out}, &out)
	if err == nil {
		t.Fatal("two seedable connections and no --connection must be refused")
	}
	for _, want := range []string{"app-postgres", "billing-postgres", "--connection"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should name %q: %v", want, err)
		}
	}
}

// A KV store beside a database is not a candidate and is not DIALLED to find
// out. A bundle owning both would otherwise fail its seeding step whenever the
// KV store happened to be down — on a question whose answer never depended on
// it.
func TestFixturesApply_AKVConnectionIsNotACandidate(t *testing.T) {
	rec := &recordingSeeder{}
	withSeeder(t, rec)
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "base",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}})
	lock := writeLock(t,
		migrate.LockConnection{Name: "app-postgres", TargetMigrationID: "ts-1"},
		migrate.LockConnection{Name: "app-redis", TargetMigrationID: "ts-2"})
	var out strings.Builder
	if err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Getenv: envMap{
			"W17_TARGET_APP_POSTGRES": "postgres://x/app",
			"W17_TARGET_APP_REDIS":    "redis://x:6379",
		}.get, Out: &out}, &out); err != nil {
		t.Fatalf("a KV store beside a database must not make the choice ambiguous: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("the postgres connection was not seeded: %+v", rec.calls)
	}
}

// The chosen connection's store has the last word. The DSN filter narrows
// candidates; whether a store can take a seed is the applier's to answer.
func TestFixturesApply_ExplicitConnectionThatCannotSeedIsRefused(t *testing.T) {
	withSeeder(t, kvOnlyApplier{})
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "base",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}})
	lock := writeLock(t, migrate.LockConnection{Name: "app-redis", TargetMigrationID: "ts-1"})
	var out strings.Builder
	err := runFixtures(context.Background(),
		[]string{"apply", "--lock", lock, "--fixtures", dir, "--connection", "app-redis"},
		Options{Getenv: envMap{"W17_TARGET_APP_REDIS": "redis://x:6379"}.get, Out: &out}, &out)
	if err == nil {
		t.Fatal("a store with no seed support must be refused")
	}
	if !strings.Contains(err.Error(), "app-redis") {
		t.Errorf("refusal should name the connection: %v", err)
	}
}

// --dry-run changes nothing. A command that reports what it would do and does
// it anyway makes every rehearsal a live run.
func TestFixturesApply_DryRunTouchesNoStore(t *testing.T) {
	rec := &recordingSeeder{}
	withSeeder(t, rec)
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "base",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}})
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres", TargetMigrationID: "ts-1"})
	var out strings.Builder
	if err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir, "--dry-run"},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out); err != nil {
		t.Fatalf("runFixtures: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("--dry-run seeded the store: %+v", rec.calls)
	}
	if !strings.Contains(out.String(), "app/base") {
		t.Errorf("--dry-run should name what it would apply:\n%s", out.String())
	}
}

// A missing artefact directory says which step did not run. "no such file or
// directory" leaves the reader guessing at a path they never chose.
func TestFixturesApply_MissingArtefactDirectoryNamesTheRenderStep(t *testing.T) {
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres", TargetMigrationID: "ts-1"})
	var out strings.Builder
	err := runFixtures(context.Background(),
		[]string{"apply", "--lock", lock, "--fixtures", filepath.Join(t.TempDir(), "absent")},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out)
	if err == nil {
		t.Fatal("a missing artefact directory must be an error")
	}
	for _, want := range []string{"--fetch", "render"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic should point at the render step (%q): %v", want, err)
		}
	}
}

// `migrate fixtures` was the shape this command had for an afternoon. Anyone
// who read that version, or a script written against it, gets pointed at the
// real one instead of "unknown subcommand".
func TestMigrate_FixturesAsASubcommandIsSteeredToTheRealOne(t *testing.T) {
	err := Main(context.Background(), []string{"fixtures"}, Options{Out: &strings.Builder{}})
	if err == nil {
		t.Fatal("migrate fixtures should not be accepted")
	}
	if !strings.Contains(err.Error(), "fixtures apply") {
		t.Errorf("error should name the command that exists: %v", err)
	}
}

// Dispatch is what a generated main calls, and it answers for every reserved
// root — the seam that made the mixed bundle's missing branch impossible to
// repeat.
func TestDispatch_AnswersForEveryReservedRoot(t *testing.T) {
	for _, root := range []string{"migrate", "fixtures"} {
		handled, _ := Dispatch(context.Background(), []string{root, "--help"}, Options{Out: &strings.Builder{}})
		if !handled {
			t.Errorf("Dispatch did not claim %q", root)
		}
	}
	if handled, _ := Dispatch(context.Background(), []string{"serve"}, Options{}); handled {
		t.Error("Dispatch claimed a word that is not reserved")
	}
	if handled, _ := Dispatch(context.Background(), nil, Options{}); handled {
		t.Error("Dispatch claimed an empty argv")
	}
}

// envMap is a Getenv the tests drive, so a run never needs a process-global
// t.Setenv and two of them can run at once.
type envMap map[string]string

func (e envMap) get(k string) string { return e[k] }

var _ = os.Getenv

// A connection with no pinned migration is still seeded.
//
// Migrating skips it, correctly: no pin means nothing to apply. Seeding is a
// different question — the database exists and the fixture belongs in it — and
// borrowing the migration rule made `fixtures apply` report that the bundle
// served no connection at all on a project that had simply never pushed a
// schema. That project was the example this whole change exists to seed, and
// only running it in a real stack surfaced it: every test here pinned.
func TestFixturesApply_AnUnpinnedConnectionIsStillSeeded(t *testing.T) {
	rec := &recordingSeeder{}
	withSeeder(t, rec)
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "base",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}})
	// No target_migration_id anywhere — the shape of a lock in a project
	// that has generated code but never pushed a schema.
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres"})
	var out strings.Builder
	if err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out); err != nil {
		t.Fatalf("an unpinned connection must still be seedable: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("nothing was seeded: %+v", rec.calls)
	}
}

// A connection the run was given no DSN for drops out instead of failing the
// run. A bundle serving a database and a KV store is handed one DSN when only
// the database is being seeded; demanding the other refuses the run over a
// store nobody was going to write to.
func TestFixturesApply_AConnectionWithNoDSNIsSkippedNotFatal(t *testing.T) {
	rec := &recordingSeeder{}
	withSeeder(t, rec)
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "base",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}})
	lock := writeLock(t,
		migrate.LockConnection{Name: "app-postgres"},
		migrate.LockConnection{Name: "app-redis"})
	var out strings.Builder
	if err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out); err != nil {
		t.Fatalf("a connection with no DSN must not fail the run: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("the connection that DID have a DSN was not seeded: %+v", rec.calls)
	}
}

// But when nothing is left, the refusal names what was dropped for want of a
// DSN. A connection skipped silently reads as one the bundle does not serve,
// and sends the reader to the lock instead of to the variable they did not set.
func TestFixturesApply_NothingSeedableNamesTheUnsetVariables(t *testing.T) {
	withSeeder(t, &recordingSeeder{})
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "base",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO base")}})
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres"})
	var out strings.Builder
	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Getenv: envMap{}.get, Out: &out}, &out)
	if err == nil {
		t.Fatal("a run with no reachable store must fail")
	}
	if !strings.Contains(err.Error(), "W17_TARGET_APP_POSTGRES") {
		t.Errorf("refusal should name the variable that was not set: %v", err)
	}
}
