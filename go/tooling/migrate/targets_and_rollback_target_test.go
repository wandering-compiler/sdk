package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// A connection listed twice used to be planned once per entry, so its chain
// was applied twice. It is refused before anything is opened.
func TestRun_RefusesAConnectionListedTwice(t *testing.T) {
	m := mkMig("20260101T000000Z", "main", "SELECT 1;")
	dir := seedDir(t, m)
	ct := migrate.ConnTarget{Connection: "main", TargetMigrationID: m.GetId(), TargetContentSha256: m.GetContentSha256()}
	st := stub.New()
	err := migrate.Run(context.Background(), migrate.Config{
		Targets:       []migrate.ConnTarget{ct, {Connection: "other"}, ct},
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return st, nil },
		Out:           &strings.Builder{},
	})
	if err == nil || !strings.Contains(err.Error(), "listed more than once") {
		t.Fatalf("duplicate connection not refused: %v", err)
	}
	if n := len(st.Calls()); n != 0 {
		t.Errorf("%d migration(s) applied despite the refusal", n)
	}

	_, err = migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets:       []migrate.ConnTarget{ct, ct},
		MigrationsDir: dir,
		ApplierFor:    func(string) (migrate.Applier, error) { return st, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "listed more than once") {
		t.Fatalf("rollback: duplicate connection not refused: %v", err)
	}
}

// `rollback --to` compares ids lexically, so an id that names no migration
// still selects a range; a typo sorting low selected everything.
func TestPlanRollback_RefusesAnUnknownTarget(t *testing.T) {
	m1 := mkMig("20260101T000000Z", "main", "SELECT 1;")
	m2 := mkMig("20260102T000000Z", "main", "SELECT 2;")
	dir := seedDir(t, m1, m2)
	st := stub.New()
	st.Head = m2.GetId()
	plan := func(to string) ([]migrate.Pending, error) {
		return migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
			Targets:       []migrate.ConnTarget{{Connection: "main"}},
			MigrationsDir: dir,
			ApplierFor:    func(string) (migrate.Applier, error) { return st, nil },
			ToMigrationID: to,
		})
	}

	p, err := plan("20250102T000000Z") // typo'd year
	if err == nil || !strings.Contains(err.Error(), "is not the id of any migration") {
		t.Fatalf("unknown rollback target not refused: err=%v planned=%d", err, len(p))
	}
	if len(p) != 0 {
		t.Errorf("%d rollback(s) planned despite the refusal", len(p))
	}

	// A real id keeps it and rolls back what is above; an explicit empty
	// target still means everything.
	if p, err := plan(m1.GetId()); err != nil || len(p) != 1 || p[0].Migration.GetId() != m2.GetId() {
		t.Fatalf("--to %s: err=%v planned=%v", m1.GetId(), err, p)
	}
	if p, err := plan(""); err != nil || len(p) != 2 {
		t.Fatalf("--to \"\": err=%v planned=%d", err, len(p))
	}
}
