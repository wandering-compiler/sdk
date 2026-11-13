package migrate_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// A51-35 (T1-1 pass #51): schema prep ran `CREATE SCHEMA IF NOT EXISTS` for
// every namespace a pending migration declares, and Postgres checks CREATE on
// the database BEFORE it looks whether the schema exists — so a
// least-privilege deploy role (ALL on its schema, no CREATE on the database)
// was refused on every deploy, the schema there all along. Prep creates only
// the namespaces that are missing.
func TestApply_SchemaPrepLeavesAnExistingSchemaToALeastPrivilegeRole_Live(t *testing.T) {
	dsn := fakePGDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	role := fmt.Sprintf("least_%d", time.Now().UnixNano())
	for _, q := range []string{
		"CREATE SCHEMA billing",
		"CREATE ROLE " + role + " LOGIN PASSWORD 'pw'",
		"GRANT ALL ON SCHEMA billing TO " + role,
		"GRANT ALL ON SCHEMA public TO " + role,
		"REVOKE CREATE ON DATABASE " + admin.Config().Database + " FROM PUBLIC",
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		if a, err := pgx.Connect(context.Background(), dsn); err == nil {
			_, _ = a.Exec(context.Background(), "DROP OWNED BY "+role)
			_ = a.Close(context.Background())
		}
		// The database is dropped by fakePGDatabase's cleanup; the role is
		// cluster-wide.
		if a, err := pgx.Connect(context.Background(), strings.Replace(dsn, "/"+admin.Config().Database, "/postgres", 1)); err == nil {
			_, _ = a.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
			_ = a.Close(context.Background())
		}
	})
	cfg := admin.Config()
	leastDSN := fmt.Sprintf("postgres://%s:pw@%s:%d/%s?sslmode=disable", role, cfg.Host, cfg.Port, cfg.Database)

	m := &applyfetchpb.Migration{
		Id:         "20260101T000000Z",
		Connection: "main",
		UpSql: `BEGIN;
			CREATE TABLE IF NOT EXISTS w17_migrations (timestamp TIMESTAMPTZ PRIMARY KEY, content_sha256 TEXT NOT NULL, post_tx_complete BOOLEAN NOT NULL DEFAULT true);
			CREATE TABLE "billing"."invoices" (id BIGINT);
			INSERT INTO w17_migrations VALUES ('2026-01-01T00:00:00Z', 'aa', true);
			COMMIT;`,
		DownSql:      `DROP TABLE "billing"."invoices";`,
		ManifestJson: `{"required_schemas":["billing"]}`,
	}
	m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", m.GetManifestJson())
	dir := seedDirUnchained(t, m)
	err = migrate.Run(ctx, migrate.Config{
		Targets:       []migrate.ConnTarget{{Connection: "main", TargetMigrationID: m.GetId(), TargetContentSha256: m.GetContentSha256()}},
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return postgres.New(ctx, leastDSN) },
		Out:           &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("a role with ALL on the existing schema billing and no CREATE on the database: %v", err)
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM billing.invoices`).Scan(&n); err != nil {
		t.Fatalf("billing.invoices after the apply: %v", err)
	}

	// The control: a namespace that does NOT exist is still created — and
	// for this role that is refused, naming the privilege.
	m2 := &applyfetchpb.Migration{
		Id:         "20260102T000000Z",
		Connection: "main",
		UpSql: `BEGIN;
			CREATE TABLE "ledger"."entries" (id BIGINT);
			INSERT INTO w17_migrations VALUES ('2026-01-02T00:00:00Z', 'bb', true);
			COMMIT;`,
		DownSql:           `DROP TABLE "ledger"."entries";`,
		ManifestJson:      `{"required_schemas":["billing","ledger"]}`,
		PrevContentSha256: m.GetContentSha256(),
	}
	m2.ContentSha256 = migrate.ContentHash(m2.GetUpSql(), "", "", m2.GetDownSql(), m.GetContentSha256(), nil, "", m2.GetManifestJson())
	dir = seedDirUnchained(t, m, m2)
	err = migrate.Run(ctx, migrate.Config{
		Targets:       []migrate.ConnTarget{{Connection: "main", TargetMigrationID: m2.GetId(), TargetContentSha256: m2.GetContentSha256()}},
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return postgres.New(ctx, leastDSN) },
		Out:           &strings.Builder{},
	})
	if err == nil || !strings.Contains(err.Error(), "ledger") || !strings.Contains(err.Error(), "CREATE on the database") {
		t.Fatalf("a missing namespace for a role without CREATE on the database: %v, want a refusal naming ledger and the privilege", err)
	}
}

// A51-35, the creating leg: a missing namespace — with a quote, an
// apostrophe and a dollar in its name — is created, and a second run leaves
// it be.
func TestApply_SchemaPrepCreatesAMissingSchema_Live(t *testing.T) {
	dsn := fakePGDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const ns = `we"i'rd$x`
	m := &applyfetchpb.Migration{
		Id:         "20260101T000000Z",
		Connection: "main",
		UpSql: `BEGIN;
			CREATE TABLE IF NOT EXISTS w17_migrations (timestamp TIMESTAMPTZ PRIMARY KEY, content_sha256 TEXT NOT NULL, post_tx_complete BOOLEAN NOT NULL DEFAULT true);
			CREATE TABLE "we""i'rd$x"."t" (id BIGINT);
			INSERT INTO w17_migrations VALUES ('2026-01-01T00:00:00Z', 'aa', true);
			COMMIT;`,
		DownSql:      `DROP TABLE "we""i'rd$x"."t";`,
		ManifestJson: `{"required_schemas":["we\"i'rd$x"]}`,
	}
	m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", m.GetManifestJson())
	dir := seedDirUnchained(t, m)
	for i := 0; i < 2; i++ {
		if err := migrate.Run(ctx, migrate.Config{
			Targets:       []migrate.ConnTarget{{Connection: "main", TargetMigrationID: m.GetId(), TargetContentSha256: m.GetContentSha256()}},
			MigrationsDir: dir,
			ApplierFor:    func(string) (migrate.Applier, error) { return postgres.New(ctx, dsn) },
			Out:           &strings.Builder{},
		}); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	probe, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close(context.Background()) }()
	var n int
	if err := probe.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = $1`, ns).Scan(&n); err != nil || n != 1 {
		t.Fatalf("namespace %q: %d (%v), want 1", ns, n, err)
	}
}
