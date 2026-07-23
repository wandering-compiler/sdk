package stub_test

import (
	"context"
	"errors"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/stub"
)

func TestApply_RecordsCalls(t *testing.T) {
	a := stub.New()
	defer func() { _ = a.Close() }()

	migs := []*applyfetchpb.Migration{
		{Id: "ts-1"}, {Id: "ts-2"}, {Id: "ts-3"},
	}
	for _, m := range migs {
		if err := a.Apply(context.Background(), m); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	got := a.Calls()
	if len(got) != 3 {
		t.Fatalf("Calls len = %d, want 3", len(got))
	}
	for i, m := range got {
		if m.GetId() != migs[i].GetId() {
			t.Errorf("[%d] id = %q, want %q", i, m.GetId(), migs[i].GetId())
		}
	}
}

func TestApply_FailOnMatches(t *testing.T) {
	a := stub.New()
	a.FailOn = "ts-2"
	a.FailErr = errors.New("boom")

	if err := a.Apply(context.Background(), &applyfetchpb.Migration{Id: "ts-1"}); err != nil {
		t.Errorf("ts-1: unexpected err %v", err)
	}
	err := a.Apply(context.Background(), &applyfetchpb.Migration{Id: "ts-2"})
	if err == nil || err.Error() != "boom" {
		t.Errorf("ts-2: expected boom, got %v", err)
	}
}

func TestClose_NoOp(t *testing.T) {
	a := stub.New()
	if err := a.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

// TestStub_AppliedHead — Head + HeadErr round-trip; tests
// that pre-populate them simulate the DB-side cutoff.
func TestStub_AppliedHead(t *testing.T) {
	a := stub.New()
	a.Head = "20260430T120000Z"
	got, err := a.AppliedHead(context.Background())
	if err != nil {
		t.Fatalf("AppliedHead: %v", err)
	}
	if got != "20260430T120000Z" {
		t.Errorf("got %q, want configured head", got)
	}

	a.HeadErr = errors.New("db gone")
	if _, err := a.AppliedHead(context.Background()); err == nil {
		t.Error("expected error from HeadErr")
	}
}

// TestStub_RollbackRecording — Rollback records calls;
// RollbackCalls returns them in order; FailOn matching id
// surfaces FailErr.
func TestStub_RollbackRecording(t *testing.T) {
	a := stub.New()
	migs := []*applyfetchpb.Migration{{Id: "m1"}, {Id: "m2"}, {Id: "m3"}}
	for _, m := range migs {
		if err := a.Rollback(context.Background(), m); err != nil {
			t.Errorf("Rollback %s: %v", m.GetId(), err)
		}
	}
	got := a.RollbackCalls()
	if len(got) != 3 {
		t.Fatalf("RollbackCalls: got %d, want 3", len(got))
	}
	for i, m := range got {
		if m.GetId() != migs[i].GetId() {
			t.Errorf("rollback[%d] = %q, want %q", i, m.GetId(), migs[i].GetId())
		}
	}
}

// TestStub_RollbackFailOn — FailOn match → FailErr.
func TestStub_RollbackFailOn(t *testing.T) {
	a := stub.New()
	a.FailOn = "boom"
	a.FailErr = errors.New("simulated rollback failure")
	if err := a.Rollback(context.Background(), &applyfetchpb.Migration{Id: "ok"}); err != nil {
		t.Errorf("non-matching id should pass; got %v", err)
	}
	if err := a.Rollback(context.Background(), &applyfetchpb.Migration{Id: "boom"}); err == nil {
		t.Error("matching FailOn should error")
	}
}
