package s3

import (
	"encoding/json"
	"testing"
)

// TestDataCursor_HasAndAdd — pure-logic unit cover for the
// op-completion bookkeeping struct used by Phase E v2.1
// resumability. add is idempotent (re-adding doesn't grow the
// list); has reports membership.
func TestDataCursor_HasAndAdd(t *testing.T) {
	c := &dataCursor{}

	if c.has(0) {
		t.Errorf("empty cursor should not report op 0 complete")
	}
	c.add(0)
	if !c.has(0) {
		t.Errorf("after add(0), has(0) should be true")
	}

	// Idempotency: re-adding doesn't double the list.
	c.add(0)
	if got := len(c.CompletedOps); got != 1 {
		t.Errorf("re-adding 0 grew list to %d entries; want 1", got)
	}

	c.add(2)
	if !c.has(2) || c.has(1) {
		t.Errorf("after add(0)+add(2): want has(0)+has(2)+!has(1); got %+v", c.CompletedOps)
	}
}

// TestDataCursor_JSONRoundTrip — a cursor written to JSON +
// read back yields the same membership. Guards the
// Marshal/Unmarshal path that loadCursor + saveCursor rely
// on.
func TestDataCursor_JSONRoundTrip(t *testing.T) {
	in := &dataCursor{CompletedOps: []int{0, 2, 5}}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out dataCursor
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, want := range []int{0, 2, 5} {
		if !out.has(want) {
			t.Errorf("round-trip lost op %d (got %+v)", want, out.CompletedOps)
		}
	}
	if out.has(1) || out.has(3) || out.has(4) {
		t.Errorf("round-trip introduced phantom ops; got %+v", out.CompletedOps)
	}
}

// TestDataCursor_EmptyJSONIsEmptyCursor — explicitly null /
// missing completed_ops decodes to a fresh cursor with no
// completed ops (matches the loadCursor missing-object path
// which builds an empty cursor and then the first op runs).
func TestDataCursor_EmptyJSONIsEmptyCursor(t *testing.T) {
	var out dataCursor
	if err := json.Unmarshal([]byte(`{}`), &out); err != nil {
		t.Fatalf("Unmarshal {}: %v", err)
	}
	if len(out.CompletedOps) != 0 {
		t.Errorf("empty JSON should yield empty cursor; got %+v", out.CompletedOps)
	}
	if out.has(0) {
		t.Errorf("empty cursor should not report any op complete")
	}
}
