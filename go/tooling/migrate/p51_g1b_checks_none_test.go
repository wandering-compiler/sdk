package migrate_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// A51-30 (T1-1 pass #51): `--checks` was a silent no-op for a migration
// that carries no checks — an older console (no field), a digest predating
// check binding, or a MySQL / SQLite migration whose every probe the console
// could not port (check_not_portable, in the plan only). The operator who set
// W17_MIGRATE_CHECKS=1 read "applied" as "guarded". The run now says so, and
// names the probes the console left out.
func TestRun_ChecksSayWhenNoneWereServed(t *testing.T) {
	m := mkMig("ts-1", "main", "ALTER TABLE `c` MODIFY COLUMN `n` DECIMAL(10, 1) NULL;")
	m.ManifestJson = `{"risk_findings":[{"op_kind":"check_not_portable","severity":"LOW","rationale":"the pre-apply check c_n_scale_narrow is written in Postgres SQL that has no MYSQL equivalent, so …"},{"op_kind":"check_not_portable","rationale":"the pre-apply check down__c_n_scale is written in Postgres SQL that has no MYSQL equivalent, so …"},{"op_kind":"carrier_change","rationale":"x"}],"planner_version":1}`
	m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", m.GetManifestJson())
	dir := seedDir(t, m)
	a := &checkingApplier{Applier: stub.New()}
	var out bytes.Buffer
	if err := migrate.Run(context.Background(), migrate.Config{
		Targets: targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}), MigrationsDir: dir, Out: &out,
		ApplierFor: func(string) (migrate.Applier, error) { return a, nil },
		Checks:     true,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no pre-apply check") || !strings.Contains(got, "c_n_scale_narrow") {
		t.Errorf("--checks over a migration whose probe the console could not port says nothing about it:\n%s", got)
	}
	if strings.Contains(got, "down__c_n_scale") {
		t.Errorf("an apply names a rollback probe:\n%s", got)
	}

	// No manifest at all: still not silent.
	m2 := mkMig("ts-2", "main", "CREATE TABLE d (id BIGINT);")
	dir = seedDir(t, m2)
	out.Reset()
	if err := migrate.Run(context.Background(), migrate.Config{
		Targets: targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()}), MigrationsDir: dir, Out: &out,
		ApplierFor: func(string) (migrate.Applier, error) { return &checkingApplier{Applier: stub.New()}, nil },
		Checks:     true,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "no pre-apply check") {
		t.Errorf("--checks over a migration with no checks says nothing:\n%s", out.String())
	}
}
