// Package stub is the test-only Applier impl that records every
// Apply call without touching any real backend. Tests use it to
// drive the orchestrator end-to-end (lock → registry → apply →
// lock-update → record) and assert the apply sequence landed
// without a Docker dependency.
package stub

import (
	"context"
	"sync"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Applier is the in-memory stub. New() returns one; the
// orchestrator calls Apply / Rollback for each migration;
// Calls() / RollbackCalls() expose the recorded sequences for
// assertions.
type Applier struct {
	mu        sync.Mutex
	calls     []*applyfetchpb.Migration
	rollbacks []*applyfetchpb.Migration

	// FailOn is the migration ID at which Apply returns FailErr
	// (simulates a mid-list dialect error). Empty = always
	// succeed. Applies to both Apply + Rollback paths.
	FailOn  string
	FailErr error

	// Head is what AppliedHead returns. Tests pre-populate to
	// simulate a partially-applied DB. Empty = fresh DB.
	Head string

	// HeadErr, when non-nil, makes AppliedHead return this error
	// (simulates a DB query failure during plan).
	HeadErr error
}

// New returns a fresh stub Applier. Compile-time check the impl
// satisfies the migrate.Applier contract.
func New() *Applier {
	return &Applier{}
}

var _ migrate.Applier = (*Applier)(nil)

// AppliedHead returns the configured Head + HeadErr. Tests
// pre-populate Head to simulate the DB-side cutoff that the
// orchestrator uses to filter pending.
func (a *Applier) AppliedHead(_ context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Head, a.HeadErr
}

// Apply records the migration + returns FailErr when its id
// matches FailOn (otherwise nil).
func (a *Applier) Apply(_ context.Context, m *applyfetchpb.Migration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, m)
	if a.FailOn != "" && m.GetId() == a.FailOn {
		return a.FailErr
	}
	return nil
}

// Rollback records the migration + returns FailErr when its id
// matches FailOn (otherwise nil). Mirrors Apply semantics.
func (a *Applier) Rollback(_ context.Context, m *applyfetchpb.Migration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rollbacks = append(a.rollbacks, m)
	if a.FailOn != "" && m.GetId() == a.FailOn {
		return a.FailErr
	}
	return nil
}

// Close is a no-op (stub holds no resources).
func (a *Applier) Close() error { return nil }

// Calls returns the recorded apply sequence in call order.
// Test-only inspection helper.
func (a *Applier) Calls() []*applyfetchpb.Migration {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*applyfetchpb.Migration, len(a.calls))
	copy(out, a.calls)
	return out
}

// RollbackCalls returns the recorded rollback sequence (newest
// rolled back first if the orchestrator did things right).
func (a *Applier) RollbackCalls() []*applyfetchpb.Migration {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*applyfetchpb.Migration, len(a.rollbacks))
	copy(out, a.rollbacks)
	return out
}
