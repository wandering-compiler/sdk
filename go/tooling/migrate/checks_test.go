package migrate_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// checkingApplier is the stub applier with the CheckRunner capability: each
// check's count is looked up by its SQL.
type checkingApplier struct {
	*stub.Applier
	counts map[string]int64
	ran    []string
}

func (c *checkingApplier) RunCheck(_ context.Context, sql string) (int64, error) {
	c.ran = append(c.ran, sql)
	return c.counts[sql], nil
}

func checkedMigration() *applyfetchpb.Migration {
	m := mkMig("ts-1", "main", "ALTER TABLE c ADD CONSTRAINT c_fk FOREIGN KEY (pid) REFERENCES p (id);")
	m.Checks = []*applyfetchpb.Check{
		{Name: "fk_add_c_pid", Sql: `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`, Rationale: "every child row holding a value is an orphan"},
		{Name: "down_only", Sql: "SELECT 99", Down: true},
	}
	return m
}

func runChecked(t *testing.T, checks bool, counts map[string]int64) (*checkingApplier, string, error) {
	t.Helper()
	m := checkedMigration()
	dir := seedDir(t, m)
	a := &checkingApplier{Applier: stub.New(), counts: counts}
	var out bytes.Buffer
	err := migrate.Run(context.Background(), migrate.Config{
		Targets: targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}), MigrationsDir: dir, Out: &out,
		ApplierFor: func(string) (migrate.Applier, error) { return a, nil },
		Checks:     checks,
	})
	return a, out.String(), err
}

// --checks refuses a migration whose forward check finds rows, naming the
// check, the count and the rationale, and applies nothing.
func TestRun_ChecksRefuseAViolatingMigration(t *testing.T) {
	a, _, err := runChecked(t, true, map[string]int64{`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`: 3})
	if err == nil {
		t.Fatal("applied over a check that found rows")
	}
	for _, want := range []string{"fk_add_c_pid", "3 row(s)", "every child row holding a value is an orphan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if len(a.Calls()) != 0 {
		t.Errorf("the migration ran after its check refused it: %v", a.Calls())
	}
}

// A check that finds nothing lets the migration through; a down check is not
// run before an apply.
func TestRun_ChecksPassAndSkipDownChecks(t *testing.T) {
	a, out, err := runChecked(t, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(a.Calls()) != 1 || len(a.ran) != 1 || strings.Contains(a.ran[0], "99") {
		t.Fatalf("calls %v, checks run %q", a.Calls(), a.ran)
	}
	if !strings.Contains(out, "check fk_add_c_pid passed") {
		t.Errorf("output does not report the check: %s", out)
	}
}

// Off by default: without Checks no probe runs and the migration applies.
func TestRun_ChecksAreOptIn(t *testing.T) {
	a, _, err := runChecked(t, false, map[string]int64{`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`: 3})
	if err != nil || len(a.ran) != 0 || len(a.Calls()) != 1 {
		t.Fatalf("err %v, checks run %q, calls %v", err, a.ran, a.Calls())
	}
}

// --checks on a store that cannot run checks refuses the migration carrying
// some rather than skipping them as if they passed.
func TestRun_ChecksRefuseWhereTheyCannotRun(t *testing.T) {
	m := checkedMigration()
	dir := seedDir(t, m)
	a := stub.New()
	err := migrate.Run(context.Background(), migrate.Config{
		Targets: targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}), MigrationsDir: dir,
		ApplierFor: func(string) (migrate.Applier, error) { return a, nil },
		Checks:     true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot run") || len(a.Calls()) != 0 {
		t.Fatalf("err %v, calls %v", err, a.Calls())
	}
}
