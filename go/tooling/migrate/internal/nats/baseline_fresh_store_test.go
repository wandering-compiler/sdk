package nats_test

import (
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// renderedBaselineNATS is, byte for byte, what the console renders as the
// NATS baseline ledger write — `applied.BaselineOnly(applied.NATS(), ts, hash)`
// for ts "20260910T120000Z" and hash 0x01. This module cannot import the
// renderer, so the string is shared by hand; the renderer's own test
// (TestBaselineNatsRendersWhatTheApplierRuns) pins the same bytes on its side.
const renderedBaselineNATS = "nats kv add w17-migrations\n\nnats kv put w17-migrations 20260910T120000Z 01"

// A baseline records the migration a BUILT store is at. That store has never
// run the initial migration, so it has no `w17-migrations` bucket — and the
// SDK resolves the bucket for a `kv put` with KeyValue, which never creates
// it. The baseline must apply on a fresh store (and again on one that has the
// bucket, the retry after a failed boot).
func TestBaselineOnly_FreshStore_Live(t *testing.T) {
	a, _ := liveApplier(t)
	ctx := liveCtx(t)
	m := &applyfetchpb.Migration{Id: "20260910T120000Z", UpSql: renderedBaselineNATS}
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("the baseline ledger write failed on a fresh store: %v", err)
	}
	head, err := a.AppliedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != "20260910T120000Z" {
		t.Fatalf("head after the baseline = %q, want 20260910T120000Z", head)
	}
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("the baseline ledger write is not re-runnable on a store that has the bucket: %v", err)
	}
}
