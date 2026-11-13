package migrate_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// pendingCheckingApplier is a checking applier on which the migration is
// half-applied (PhasePending): its in-tx half committed, its skirt stopped.
// A down probe errors, as it does on PostgreSQL when it names an identifier
// the unfinished skirt never produced.
type pendingCheckingApplier struct {
	*checkingApplier
}

func (p *pendingCheckingApplier) MigrationPhase(context.Context, string) (migrate.Phase, error) {
	return migrate.PhasePending, nil
}

func (p *pendingCheckingApplier) ApplyPostTx(context.Context, *applyfetchpb.Migration) error {
	return nil
}

func (p *pendingCheckingApplier) RunCheck(ctx context.Context, sql, body string) (int64, error) {
	if sql == "SELECT 99" {
		p.ran = append(p.ran, sql)
		return 0, errors.New(`relation "old_t" does not exist (SQLSTATE 42P01)`)
	}
	return p.checkingApplier.RunCheck(ctx, sql, body)
}

// T1-1 pass #51, Q51-3 (C51-8 + A51-34's second half) — `rollback --checks` of
// a half-applied migration ran its down probes, which the planner renders
// against the fully-applied schema; an unfinished skirt does not have it, the
// probe could not run, and --checks refused the very rollback the half-applied
// migration's failure text names. As on apply, the probes of a half-applied
// migration are not run; the run says so.
func TestRollback_ChecksSkipAHalfAppliedMigrationsDownProbes(t *testing.T) {
	m := checkedMigration()
	dir := seedDir(t, m)
	a := &pendingCheckingApplier{&checkingApplier{Applier: stub.New()}}
	var out bytes.Buffer
	err := migrate.RunRollback(context.Background(), migrate.RollbackConfig{
		Targets: targets(lockTarget{"main", m.GetId(), m.GetContentSha256()}), MigrationsDir: dir, Out: &out,
		ApplierFor:    func(string) (migrate.Applier, error) { return a, nil },
		ToMigrationID: "",
		Checks:        true,
	})
	if err != nil {
		t.Fatalf("rollback --checks of a half-applied migration: %v", err)
	}
	if len(a.ran) != 0 {
		t.Errorf("the down probes of a half-applied migration ran: %q", a.ran)
	}
	if len(a.RollbackCalls()) != 1 {
		t.Errorf("rollbacks %v, want the half-applied migration rolled back", a.RollbackCalls())
	}
	if !strings.Contains(out.String(), "half-applied") || !strings.Contains(out.String(), "down_only") {
		t.Errorf("the run does not say why the checks did not run: %s", out.String())
	}
}
