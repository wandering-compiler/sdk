package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// halfAppliedUnderLock answers "nothing half-applied" until the run lock is
// taken and ts-1 after: another applier carrying a different history (a
// rolling deploy of an older binary) committed ts-1's transaction and died
// in its skirt between this run's Plan and its lock.
type halfAppliedUnderLock struct {
	*resumableStub
	locked bool
}

func (f *halfAppliedUnderLock) PendingMigrations(context.Context) ([]string, error) {
	if f.locked {
		return []string{"ts-1"}, nil
	}
	return nil, nil
}

func (f *halfAppliedUnderLock) AcquireRunLock(context.Context) (migrate.RunLock, error) {
	f.locked = true
	return releasedLock{}, nil
}

type releasedLock struct{}

func (releasedLock) Release(context.Context) error { return nil }

// Plan's half-applied refusal is asked again under the run lock: the row that
// appeared while this run waited is refused, by name, before the replacement
// is built on top of its committed DDL.
func TestRun_HalfAppliedOffPlanIsAskedAgainUnderTheLock(t *testing.T) {
	m0 := mkMig("ts-0", "main", "CREATE TABLE a;")
	m2 := mkMig("ts-2", "main", "CREATE TABLE b2;")
	dir := seedDir(t, m0, m2)
	f := &halfAppliedUnderLock{resumableStub: newResumable()}
	f.Head = "ts-0"
	err := migrate.Run(context.Background(), migrate.Config{
		Targets: targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()}), MigrationsDir: dir,
		ApplierFor: func(string) (migrate.Applier, error) { return f, nil },
		Replaced:   map[string]map[string]string{"main": {"ts-1": "ts-2"}},
	})
	if err == nil || !strings.Contains(err.Error(), "ts-1") || !strings.Contains(err.Error(), "replaced by ts-2") {
		t.Fatalf("Run = %v, want a refusal naming the half-applied ts-1 and its replacement", err)
	}
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("%d migration(s) applied over the half-applied ts-1", n)
	}
}

// The rollback twin: a half-applied row that appeared above an unchanged head
// while the rollback waited is not in its plan, so the rollback refuses
// instead of undoing what that migration was built on.
func TestRunRollback_HalfAppliedThatAppearedUnderTheLockIsRefused(t *testing.T) {
	m0 := mkMig("ts-0", "main", "CREATE TABLE a;")
	dir := seedDir(t, m0)
	f := &halfAppliedUnderLock{resumableStub: newResumable()}
	f.Head = "ts-0"
	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets: targets(lockTarget{"main", m0.GetId(), m0.GetContentSha256()}), MigrationsDir: dir,
		ApplierFor: func(string) (migrate.Applier, error) { return f, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ts-1") {
		t.Fatalf("RunRollback = %v, want a refusal naming the half-applied ts-1", err)
	}
	if n := len(f.RollbackCalls()); n != 0 {
		t.Fatalf("%d migration(s) rolled back under the half-applied ts-1", n)
	}
}
