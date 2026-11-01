package migrate_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

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
	bodies []string
}

func (c *checkingApplier) RunCheck(_ context.Context, sql, body string) (int64, error) {
	c.ran = append(c.ran, sql)
	c.bodies = append(c.bodies, body)
	return c.counts[sql], nil
}

func checkedMigration() *applyfetchpb.Migration {
	m := mkMig("ts-1", "main", "ALTER TABLE c ADD CONSTRAINT c_fk FOREIGN KEY (pid) REFERENCES p (id);")
	m.Checks = []*applyfetchpb.Check{
		{Name: "fk_add_c_pid", Sql: `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`, Rationale: "every child row holding a value is an orphan"},
		{Name: "down_only", Sql: "SELECT 99", Down: true},
	}
	m.ContentSha256 = migrate.ContentHashWithChecks(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", "", "", m.GetChecks())
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
	// the check is validated against the body of the migration it guards
	if !strings.Contains(a.bodies[0], "ADD CONSTRAINT c_fk") {
		t.Errorf("the check ran without its migration's body: %q", a.bodies[0])
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

// A check is as trusted as the body: the digest binds it, so an artifact
// whose check was replaced, or that carries checks its digest does not
// cover, is refused before anything runs.
func TestChecks_AreBoundByTheDigest(t *testing.T) {
	m := checkedMigration()
	if !migrate.ChecksBound(m) {
		t.Fatal("a migration minted with its checks does not verify")
	}
	if err := migrate.WriteMigration(t.TempDir(), m); err != nil {
		t.Fatalf("a bound artifact is refused: %v", err)
	}
	tampered := proto.Clone(m).(*applyfetchpb.Migration)
	tampered.Checks[0].Sql = `SELECT count(*) FROM "c" WHERE false LIMIT 1`
	if migrate.ChecksBound(tampered) || migrate.WriteMigration(t.TempDir(), tampered) == nil {
		t.Error("an artifact whose check was replaced is accepted")
	}
	unbound := proto.Clone(m).(*applyfetchpb.Migration)
	unbound.ContentSha256 = migrate.ContentHash(unbound.GetUpSql(), "", "", unbound.GetDownSql(), "", nil, "", "")
	if migrate.ChecksBound(unbound) || migrate.WriteMigration(t.TempDir(), unbound) == nil {
		t.Error("an artifact carrying checks its digest does not bind is accepted")
	}
}

func runRollbackChecked(t *testing.T, checks bool, counts map[string]int64) (*checkingApplier, string, error) {
	t.Helper()
	m := checkedMigration()
	dir := seedDir(t, m)
	a := &checkingApplier{Applier: stub.New(), counts: counts}
	a.Head = m.GetId()
	var out bytes.Buffer
	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets: targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}), MigrationsDir: dir, Out: &out,
		ApplierFor:    func(string) (migrate.Applier, error) { return a, nil },
		ToMigrationID: "",
		Checks:        checks,
	})
	return a, out.String(), err
}

// rollback --checks runs a migration's DOWN checks before its down body and
// refuses the rollback when one finds rows, naming it; nothing rolls back.
func TestRollback_ChecksRefuseAViolatingRollback(t *testing.T) {
	a, _, err := runRollbackChecked(t, true, map[string]int64{"SELECT 99": 2})
	if err == nil {
		t.Fatal("rolled back over a down check that found rows")
	}
	for _, want := range []string{"rollback main/ts-1", "pre-rollback check down_only", "2 row(s)", "nothing was rolled back"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if len(a.RollbackCalls()) != 0 {
		t.Errorf("the down body ran after its check refused it: %v", a.RollbackCalls())
	}
}

// A down check that finds nothing lets the rollback through, and a forward
// check is not run before a rollback (here it would refuse).
func TestRollback_ChecksPassAndSkipForwardChecks(t *testing.T) {
	a, out, err := runRollbackChecked(t, true, map[string]int64{`SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`: 3})
	if err != nil {
		t.Fatalf("RunRollback: %v", err)
	}
	if len(a.RollbackCalls()) != 1 || len(a.ran) != 1 || a.ran[0] != "SELECT 99" {
		t.Fatalf("rollbacks %v, checks run %q", a.RollbackCalls(), a.ran)
	}
	// a down check is validated against the down body (and the up body)
	if !strings.Contains(a.bodies[0], "-- down for ts-1") || !strings.Contains(a.bodies[0], "ADD CONSTRAINT c_fk") {
		t.Errorf("the down check ran without its migration's bodies: %q", a.bodies[0])
	}
	if !strings.Contains(out, "rollback:   check down_only passed") {
		t.Errorf("output does not report the check: %s", out)
	}
}

// Off by default: without Checks no down check runs and the rollback goes on.
func TestRollback_ChecksAreOptIn(t *testing.T) {
	a, _, err := runRollbackChecked(t, false, map[string]int64{"SELECT 99": 2})
	if err != nil || len(a.ran) != 0 || len(a.RollbackCalls()) != 1 {
		t.Fatalf("err %v, checks run %q, rollbacks %v", err, a.ran, a.RollbackCalls())
	}
}

// rollback --checks on a store that cannot run checks refuses a migration
// carrying down checks instead of skipping them as if they passed.
func TestRollback_ChecksRefuseWhereTheyCannotRun(t *testing.T) {
	m := checkedMigration()
	dir := seedDir(t, m)
	a := stub.New()
	a.Head = m.GetId()
	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets: targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}), MigrationsDir: dir,
		ApplierFor: func(string) (migrate.Applier, error) { return a, nil },
		Checks:     true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot run") || len(a.RollbackCalls()) != 0 {
		t.Fatalf("err %v, rollbacks %v", err, a.RollbackCalls())
	}
}
