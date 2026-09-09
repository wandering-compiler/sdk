package migratecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// recordingApplier remembers the DDL a schema run handed it.
type recordingApplier struct{ applied []string }

func (r *recordingApplier) Apply(_ context.Context, m *applyfetchpb.Migration) error {
	r.applied = append(r.applied, m.GetUpSql())
	return nil
}
func (r *recordingApplier) Close() error                                { return nil }
func (r *recordingApplier) AppliedHead(context.Context) (string, error) { panic("not used") }
func (r *recordingApplier) Rollback(context.Context, *applyfetchpb.Migration) error {
	panic("not used")
}

func withApplier(t *testing.T, ap migrate.Applier) {
	t.Helper()
	prev := newApplierFor
	newApplierFor = func([]factory.TargetSpec) migrate.ApplierFor {
		return func(string) (migrate.Applier, error) { return ap, nil }
	}
	t.Cleanup(func() { newApplierFor = prev })
}

func planDir(t *testing.T, conns ...string) string {
	t.Helper()
	root := t.TempDir()
	p := &applyplanpb.DevApplyPlan{}
	for _, c := range conns {
		p.Migrations = append(p.Migrations, &applyplanpb.DevMigration{
			Connection: c, UpSql: "CREATE TABLE " + strings.ReplaceAll(c, "-", "_") + "_t (id INT)",
		})
	}
	if err := migrate.WriteDevPlan(root, p); err != nil {
		t.Fatalf("WriteDevPlan: %v", err)
	}
	return root
}

// The bundle applies the connections it serves and no others.
//
// A rendered plan carries every connection the PROJECT has, and two storage
// bundles in one project each hold a slice of it. A bundle reaching past its
// own would run DDL against a database it does not serve and cannot test
// against — the same rule migrate apply follows, and the reason the plan is
// narrowed here rather than left to fail on a name the factory has no DSN for.
func TestSchemaApply_AppliesOnlyTheBundlesOwnConnections(t *testing.T) {
	rec := &recordingApplier{}
	withApplier(t, rec)
	dir := planDir(t, "app-postgres", "billing-postgres")
	lock := writeLock(t,
		migrate.LockConnection{Name: "app-postgres"},
		migrate.LockConnection{Name: "billing-postgres"})
	var out strings.Builder
	err := runSchema(context.Background(), []string{"apply", "--lock", lock, "--schema", dir},
		Options{
			Connections: []string{"app-postgres"},
			Getenv:      envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get,
			Out:         &out,
		}, &out)
	if err != nil {
		t.Fatalf("runSchema: %v\n%s", err, out.String())
	}
	if len(rec.applied) != 1 {
		t.Fatalf("applied %d plan(s), want just this bundle's: %v", len(rec.applied), rec.applied)
	}
	if !strings.Contains(rec.applied[0], "app_postgres_t") {
		t.Errorf("wrong connection's DDL ran: %q", rec.applied[0])
	}
}

// --dry-run changes nothing. A command that reports what it would do and does
// it anyway makes every rehearsal a live run.
func TestSchemaApply_DryRunTouchesNoStore(t *testing.T) {
	rec := &recordingApplier{}
	withApplier(t, rec)
	dir := planDir(t, "app-postgres")
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres"})
	var out strings.Builder
	if err := runSchema(context.Background(),
		[]string{"apply", "--lock", lock, "--schema", dir, "--dry-run"},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	if len(rec.applied) != 0 {
		t.Fatalf("--dry-run applied DDL: %v", rec.applied)
	}
	if !strings.Contains(out.String(), "app-postgres") {
		t.Errorf("--dry-run should name what it would do:\n%s", out.String())
	}
}

// A missing artefact is an ERROR, not an empty plan.
//
// "No schema to apply" and "the render step never ran" are the same silence
// otherwise, and the second one leaves a database with no tables and a stack
// that fails further along with a message about neither.
func TestSchemaApply_AMissingPlanIsRefusedNotTreatedAsEmpty(t *testing.T) {
	withApplier(t, &recordingApplier{})
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres"})
	var out strings.Builder
	err := runSchema(context.Background(),
		[]string{"apply", "--lock", lock, "--schema", filepath.Join(t.TempDir(), "absent")},
		Options{Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/y"}.get, Out: &out}, &out)
	if err == nil {
		t.Fatal("a missing rendered plan must be an error")
	}
	if !strings.Contains(err.Error(), "render") {
		t.Errorf("the diagnostic should point at the render step: %v", err)
	}
}

// The artefact's bytes are pinned, for the same reason the rendered fixtures'
// are: it is a generated file that lands in a diff, and protojson varies its
// whitespace between processes on purpose.
func TestWriteDevPlan_BytesArePinned(t *testing.T) {
	root := t.TempDir()
	if err := migrate.WriteDevPlan(root, &applyplanpb.DevApplyPlan{
		Migrations: []*applyplanpb.DevMigration{{Connection: "app-postgres", UpSql: "CREATE TABLE t (id INT)"}},
	}); err != nil {
		t.Fatalf("WriteDevPlan: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "dev-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{
  "migrations": [
    {
      "connection": "app-postgres",
      "up_sql": "CREATE TABLE t (id INT)"
    }
  ]
}
`
	if string(got) != want {
		t.Errorf("artefact bytes changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// Dispatch answers for `schema` the way it answers for the others — the seam
// that makes a new built-in reach every bundle flavour at once.
func TestDispatch_ClaimsSchema(t *testing.T) {
	handled, _ := Dispatch(context.Background(), []string{"schema", "--help"}, Options{Out: &strings.Builder{}})
	if !handled {
		t.Error("Dispatch did not claim `schema`")
	}
}
