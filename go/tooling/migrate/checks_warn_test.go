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

const warnProbe = `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL AND "pid" NOT IN (SELECT "id" FROM "p") LIMIT 1`

// warnMigration is a key added over a table that may hold orphans: its probe
// warns, a second probe still refuses.
func warnMigration(refuseToo bool) *applyfetchpb.Migration {
	m := mkMig("ts-1", "main", "ALTER TABLE c ADD CONSTRAINT c_fk FOREIGN KEY (pid) REFERENCES p (id) NOT VALID;")
	m.Checks = []*applyfetchpb.Check{
		{Name: "fk_add_c_pid", Sql: warnProbe, Rationale: "child rows that reference no parent row", Severity: applyfetchpb.Check_SEVERITY_WARN},
	}
	if refuseToo {
		m.Checks = append(m.Checks, &applyfetchpb.Check{Name: "index_add_c_x", Sql: `SELECT count(*) FROM "c" WHERE "x" IS NULL LIMIT 1`, Rationale: "duplicates"})
	}
	m.ContentSha256 = migrate.ContentHashWithChecks(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", "", "", m.GetChecks())
	return m
}

func runWarn(t *testing.T, m *applyfetchpb.Migration, counts map[string]int64) (*checkingApplier, string, error) {
	t.Helper()
	dir := seedDir(t, m)
	a := &checkingApplier{Applier: stub.New(), counts: counts}
	var out bytes.Buffer
	err := migrate.Run(context.Background(), migrate.Config{
		Targets: targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}), MigrationsDir: dir, Out: &out,
		ApplierFor: func(string) (migrate.Applier, error) { return a, nil },
		Checks:     true,
	})
	return a, out.String(), err
}

// A WARN check that finds rows prints the count, its rationale and its probe,
// and the migration applies.
func TestRun_WarnCheckReportsAndApplies(t *testing.T) {
	a, out, err := runWarn(t, warnMigration(false), map[string]int64{warnProbe: 4})
	if err != nil {
		t.Fatalf("a warning check refused the migration: %v", err)
	}
	if len(a.Calls()) != 1 {
		t.Fatalf("the migration did not run after a warning: %v", a.Calls())
	}
	for _, want := range []string{"check fk_add_c_pid WARNING: 4 row(s) found", "goes ahead", "child rows that reference no parent row", warnProbe} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A WARN check finding nothing passes like any other; a REFUSE check beside it
// still refuses.
func TestRun_WarnCheckDoesNotWeakenOtherChecks(t *testing.T) {
	_, out, err := runWarn(t, warnMigration(false), nil)
	if err != nil || !strings.Contains(out, "check fk_add_c_pid passed") {
		t.Fatalf("err %v, out:\n%s", err, out)
	}
	a, _, err := runWarn(t, warnMigration(true), map[string]int64{warnProbe: 4, `SELECT count(*) FROM "c" WHERE "x" IS NULL LIMIT 1`: 1})
	if err == nil || !strings.Contains(err.Error(), "index_add_c_x") || len(a.Calls()) != 0 {
		t.Fatalf("a refusing check beside a warning one let the migration through: err %v, calls %v", err, a.Calls())
	}
}

// The severity is bound by the digest — written only when WARN, so a check
// minted without it keeps its digest — and flipping it either way is refused.
func TestChecks_SeverityIsBoundByTheDigest(t *testing.T) {
	m := warnMigration(false)
	if !migrate.ChecksBound(m) {
		t.Fatal("a migration minted with a warning check does not verify")
	}
	refuse := proto.Clone(m).(*applyfetchpb.Migration)
	refuse.Checks[0].Severity = applyfetchpb.Check_SEVERITY_REFUSE
	if migrate.ChecksBound(refuse) || migrate.WriteMigration(t.TempDir(), refuse) == nil {
		t.Error("an artifact whose warning check was turned into a refusal is accepted")
	}
	// The other way: a refusing check minted before severity existed hashes as
	// it always did, and turning it into a warning breaks the digest.
	old := checkedMigration()
	// The digest this migration had before severity existed (measured on the
	// SDK without it).
	if got := old.GetContentSha256(); got != "a094bb46620e52b9c79e80e0424763152e871e41b08110e10130f55a1420429e" {
		t.Fatalf("a check without severity no longer hashes as minted: %s", got)
	}
	warned := proto.Clone(old).(*applyfetchpb.Migration)
	warned.Checks[0].Severity = applyfetchpb.Check_SEVERITY_WARN
	if migrate.ChecksBound(warned) {
		t.Error("an artifact whose refusing check was turned into a warning is accepted")
	}
}
