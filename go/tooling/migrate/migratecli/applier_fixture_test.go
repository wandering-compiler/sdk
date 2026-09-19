package migratecli

import (
	"context"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
	"testing"
)

// recordingApplier remembers the DDL a run handed it.
type recordingApplier struct{ applied []string }

func (r *recordingApplier) Apply(_ context.Context, m *applyfetchpb.Migration) error {
	r.applied = append(r.applied, m.GetUpSql())
	return nil
}
func (r *recordingApplier) Close() error                                { return nil }
func (r *recordingApplier) AppliedHead(context.Context) (string, error) { panic("not used") }
func (r *recordingApplier) Rollback(context.Context, *applyfetchpb.Migration) error {
	panic("not used")
}

func withApplier(t *testing.T, ap migrate.Applier) {
	t.Helper()
	prev := newApplierFor
	newApplierFor = func([]factory.TargetSpec) migrate.ApplierFor {
		return func(string) (migrate.Applier, error) { return ap, nil }
	}
	t.Cleanup(func() { newApplierFor = prev })
}
