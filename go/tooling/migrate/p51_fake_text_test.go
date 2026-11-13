package migrate_test

import (
	"context"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// T1-1 pass #51, A51-36 (second half) — the refusal of --fake on a migration
// with no adopt preflight sent every operator to "fetch the migration again
// from a console that renders one (relational dialects only)". The console
// renders one for PostgreSQL only: on MySQL or SQLite no fetch ever brings
// one, and the text must say so instead of promising one.
func TestFake_NoPreflightRefusalNamesPostgreSQLOnly(t *testing.T) {
	m := &applyfetchpb.Migration{
		Id: "20260101T000000Z", Connection: "main",
		UpSql: "CREATE TABLE t(id int);", DownSql: "DROP TABLE t;",
	}
	m.ContentSha256 = migrate.ContentHashWithPreflight(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", "", "")
	run := func(a migrate.Applier) error {
		return migrate.Run(context.Background(), migrate.Config{
			Targets:       []migrate.ConnTarget{{Connection: "main", TargetMigrationID: m.GetId(), TargetContentSha256: m.GetContentSha256()}},
			MigrationsDir: seedDirUnchained(t, m),
			ApplierFor:    func(string) (migrate.Applier, error) { return a, nil },
			Fake:          true,
			Out:           &strings.Builder{},
		})
	}
	notPG := stub.New()
	notPG.NotPostgres = true
	other := run(notPG)
	if other == nil || !strings.Contains(other.Error(), "PostgreSQL only") || strings.Contains(other.Error(), "fetch the migration again") {
		t.Errorf("on a store that is not PostgreSQL the refusal promises a preflight a fetch never brings:\n%v", other)
	}
	pg := run(stub.New())
	if pg == nil || !strings.Contains(pg.Error(), "fetch the migration again") {
		t.Errorf("on PostgreSQL the refusal does not name the fetch:\n%v", pg)
	}
}
