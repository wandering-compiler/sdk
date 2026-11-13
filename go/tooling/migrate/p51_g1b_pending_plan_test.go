package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// pendingListing is a resumable stub whose ledger also answers which rows
// are half-applied (post-tx not complete), as the Postgres applier does.
type pendingListing struct {
	*resumableStub
}

func (p *pendingListing) PendingMigrations(context.Context) ([]string, error) {
	var out []string
	for id, ph := range p.phases {
		if ph == migrate.PhasePending {
			out = append(out, id)
		}
	}
	return out, nil
}

// A51-32 (T1-1 pass #51): AppliedHead leaves a half-applied (PhasePending)
// migration out, and Plan judged the head only — so a half-applied
// migration the deploy does not continue was invisible:
//
//	(b) the lock lowered below it: "nothing pending", while its in-tx DDL is
//	    committed and its skirt unfinished;
//	(a) it was replaced in the console: the replacement planned over it with
//	    no refusal (the drift gate stopped it later, blaming a hand edit).
//
// Plan now refuses a half-applied row the deploy will not resume, naming it
// and the rollback that clears it.
func TestPlan_RefusesAHalfAppliedMigrationTheDeployDoesNotContinue(t *testing.T) {
	t.Run("LockBelowIt", func(t *testing.T) {
		m0 := mkMig("ts-0", "main", "CREATE TABLE a;")
		m1 := mkMigFull("ts-1", "main", "CREATE TABLE b;", "CREATE INDEX CONCURRENTLY b_i ON b(id);", "", "-- down ts-1")
		dir := seedDir(t, m0, m1)
		ra := &pendingListing{newResumable()}
		ra.Head = "ts-0"
		ra.phases["ts-1"] = migrate.PhasePending
		_, err := migrate.Plan(context.Background(), migrate.Config{
			Targets: targets(lockTarget{"main", m0.GetId(), m0.GetContentSha256()}), MigrationsDir: dir,
			ApplierFor: func(string) (migrate.Applier, error) { return ra, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "ts-1") || !strings.Contains(err.Error(), "rollback") {
			t.Fatalf("a half-applied migration above the pinned target: %v, want a refusal naming ts-1 and the rollback", err)
		}
	})
	t.Run("Replaced", func(t *testing.T) {
		m0 := mkMig("ts-0", "main", "CREATE TABLE a;")
		m1b := mkMig("ts-2", "main", "CREATE TABLE b2;")
		dir := seedDir(t, m0, m1b)
		ra := &pendingListing{newResumable()}
		ra.Head = "ts-0"
		ra.phases["ts-1"] = migrate.PhasePending
		_, err := migrate.Plan(context.Background(), migrate.Config{
			Targets: targets(lockTarget{"main", m1b.GetId(), m1b.GetContentSha256()}), MigrationsDir: dir,
			ApplierFor: func(string) (migrate.Applier, error) { return ra, nil },
			Replaced:   map[string]map[string]string{"main": {"ts-1": "ts-2"}},
		})
		if err == nil || !strings.Contains(err.Error(), "ts-1") || !strings.Contains(err.Error(), "replaced by ts-2") {
			t.Fatalf("a half-applied migration the console replaced: %v, want a refusal naming ts-1 and its replacement", err)
		}
	})
	t.Run("OnTheChainResumes", func(t *testing.T) {
		m0 := mkMig("ts-0", "main", "CREATE TABLE a;")
		m1 := mkMigFull("ts-1", "main", "CREATE TABLE b;", "CREATE INDEX CONCURRENTLY b_i ON b(id);", "", "-- down ts-1")
		dir := seedDir(t, m0, m1)
		ra := &pendingListing{newResumable()}
		ra.Head = "ts-0"
		ra.phases["ts-1"] = migrate.PhasePending
		pend, err := migrate.Plan(context.Background(), migrate.Config{
			Targets: targets(lockTarget{"main", m1.GetId(), m1.GetContentSha256()}), MigrationsDir: dir,
			ApplierFor: func(string) (migrate.Applier, error) { return ra, nil },
		})
		if err != nil || len(pend) != 1 || pend[0].Migration.GetId() != "ts-1" {
			t.Fatalf("a half-applied migration the deploy continues: %v %v, want ts-1 pending (resumed)", pend, err)
		}
	})
}

// A51-32, live: the Postgres applier lists the half-applied rows of its
// ledger, and a deploy pinned below one is refused rather than "up to date".
func TestPlan_RefusesAHalfAppliedMigrationAboveThePin_Live(t *testing.T) {
	dsn := fakePGDatabase(t)
	ctx := context.Background()
	m0 := mkMig("20260101T000000Z", "main", "SELECT 1;")
	m1 := mkMig("20260102T000000Z", "main", "SELECT 1;")
	dir := seedDir(t, m0, m1)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE w17_migrations (timestamp TIMESTAMPTZ PRIMARY KEY, content_sha256 TEXT NOT NULL, post_tx_complete BOOLEAN NOT NULL DEFAULT true);
		INSERT INTO w17_migrations VALUES ('2026-01-01T00:00:00Z', 'aa', true), ('2026-01-02T00:00:00Z', 'bb', false);`); err != nil {
		t.Fatal(err)
	}
	_, err = migrate.Plan(ctx, migrate.Config{
		Targets: targets(lockTarget{"main", m0.GetId(), m0.GetContentSha256()}), MigrationsDir: dir,
		ApplierFor: func(string) (migrate.Applier, error) { return postgres.New(ctx, dsn) },
	})
	if err == nil || !strings.Contains(err.Error(), "20260102T000000Z") {
		t.Fatalf("Plan over a half-applied row above the pin: %v, want a refusal naming it", err)
	}
}
