package migrate_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

// resumableStub is a stub.Applier that also implements
// migrate.ResumableApplier (Q52). It reports a configured per-id phase
// and records which ids took the post-tx resume path vs the full
// Apply path (the latter via the embedded stub's Calls()).
type resumableStub struct {
	*stub.Applier
	phases      map[string]migrate.Phase
	postTxCalls []string
}

func newResumable() *resumableStub {
	return &resumableStub{Applier: stub.New(), phases: map[string]migrate.Phase{}}
}

// Compile-time check it satisfies BOTH contracts.
var _ migrate.Applier = (*resumableStub)(nil)
var _ migrate.ResumableApplier = (*resumableStub)(nil)

func (r *resumableStub) MigrationPhase(_ context.Context, id string) (migrate.Phase, error) {
	return r.phases[id], nil // missing key → PhaseFresh (zero value)
}

func (r *resumableStub) ApplyPostTx(_ context.Context, m *applyfetchpb.Migration) error {
	r.postTxCalls = append(r.postTxCalls, m.GetId())
	return nil
}

// TestRun_ResumesPendingSkirt — a migration the applier reports as
// PhasePending (its in-tx half committed in a prior crashed deploy)
// must resume via ApplyPostTx, NOT a full Apply that would re-run the
// already-committed in-tx DDL. A fresh sibling still takes the full
// Apply path.
func TestRun_ResumesPendingSkirt(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a;")     // already complete (≤ head)
	m2 := mkMigFull("ts-2", "main", "CREATE TABLE b;", // partially applied
		"CREATE INDEX CONCURRENTLY b_idx ON b(id);", "", "-- down ts-2")
	m3 := mkMig("ts-3", "main", "CREATE TABLE c;") // fresh
	dir := seedDir(t, m1, m2, m3)
	tg := targets(lockTarget{"main", m3.GetId(), m3.GetContentSha256()})

	ra := newResumable()
	ra.Head = "ts-1"                         // AppliedHead's post_tx_complete cutoff
	ra.phases["ts-2"] = migrate.PhasePending // in-tx committed, post-tx unfinished
	// ts-3 unset → PhaseFresh
	var out bytes.Buffer

	err := migrate.Run(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir, Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return ra, nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// ts-2 resumed via post-tx only; ts-3 full apply.
	if len(ra.postTxCalls) != 1 || ra.postTxCalls[0] != "ts-2" {
		t.Errorf("post-tx resume calls = %v, want [ts-2]", ra.postTxCalls)
	}
	fullApply := ra.Calls()
	if len(fullApply) != 1 || fullApply[0].GetId() != "ts-3" {
		t.Errorf("full-apply calls = %v, want [ts-3]", idsOf(fullApply))
	}
	for _, m := range fullApply {
		if m.GetId() == "ts-2" {
			t.Error("ts-2 must NOT take the full Apply path (would re-run committed in-tx DDL)")
		}
	}
	if !strings.Contains(out.String(), "resuming post-tx phase for ts-2") {
		t.Errorf("expected a resume notice for ts-2; got:\n%s", out.String())
	}
}

// TestPlanRollback_IncludesHalfAppliedAboveHead (writer-F1) — a PhasePending
// migration (in-tx half committed, skirt crashed) sits ABOVE AppliedHead's
// post_tx_complete cutoff, but rollback must still undo it: it is invisible to
// rollback otherwise, and its committed DDL + lying pending row persist.
func TestPlanRollback_IncludesHalfAppliedAboveHead(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a;")
	m2 := mkMigFull("ts-2", "main", "CREATE TABLE b;", "CREATE INDEX CONCURRENTLY b_idx ON b(id);", "", "-- down ts-2")
	dir := seedDir(t, m1, m2)
	tg := targets(lockTarget{"main", m2.GetId(), m2.GetContentSha256()})

	ra := newResumable()
	ra.Head = "ts-1"                         // AppliedHead excludes the pending ts-2
	ra.phases["ts-2"] = migrate.PhasePending // half-applied above head

	pending, err := migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "ts-1",
		ApplierFor: func(_ string) (migrate.Applier, error) { return ra, nil },
	})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if len(pending) != 1 || pending[0].Migration.GetId() != "ts-2" {
		t.Fatalf("writer-F1: half-applied ts-2 above head must be rolled back; got %+v", pending)
	}
}

// TestPlanRollback_HalfAppliedOnFreshLookingDB (writer-F1) — when the ONLY
// migration is half-applied, AppliedHead is "" (no complete row), but rollback
// must still undo it rather than report "nothing to roll back".
func TestPlanRollback_HalfAppliedOnFreshLookingDB(t *testing.T) {
	m1 := mkMigFull("ts-1", "main", "CREATE TABLE a;", "CREATE INDEX CONCURRENTLY a_idx ON a(id);", "", "-- down ts-1")
	dir := seedDir(t, m1)
	tg := targets(lockTarget{"main", m1.GetId(), m1.GetContentSha256()})

	ra := newResumable()
	ra.Head = "" // no complete row
	ra.phases["ts-1"] = migrate.PhasePending

	pending, err := migrate.PlanRollback(context.Background(), migrate.RollbackConfig{
		Targets: tg, MigrationsDir: dir, ToMigrationID: "",
		ApplierFor: func(_ string) (migrate.Applier, error) { return ra, nil },
	})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if len(pending) != 1 || pending[0].Migration.GetId() != "ts-1" {
		t.Fatalf("writer-F1: a half-applied migration on a fresh-looking DB must roll back; got %+v", pending)
	}
}

// TestRun_NonResumableUsesPlainApply — an applier WITHOUT the
// ResumableApplier capability (the bare stub) always takes the full
// Apply path; the orchestrator never consults phase state.
func TestRun_NonResumableUsesPlainApply(t *testing.T) {
	m1 := mkMig("ts-1", "main", "CREATE TABLE a;")
	dir := seedDir(t, m1)
	tg := targets(lockTarget{"main", m1.GetId(), m1.GetContentSha256()})

	bare := stub.New() // no MigrationPhase / ApplyPostTx
	var out bytes.Buffer
	if err := migrate.Run(context.Background(), migrate.Config{
		Targets: tg, MigrationsDir: dir, Out: &out,
		ApplierFor: func(_ string) (migrate.Applier, error) { return bare, nil },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := bare.Calls(); len(got) != 1 || got[0].GetId() != "ts-1" {
		t.Errorf("bare applier should see one Apply(ts-1); got %v", idsOf(got))
	}
}

func idsOf(ms []*applyfetchpb.Migration) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.GetId()
	}
	return out
}
