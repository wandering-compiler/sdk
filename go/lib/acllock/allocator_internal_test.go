package acllock

import "testing"

// sortAndDedupReserved returns nil for empty input — the empty
// branch is unreachable through Allocate (which only calls it when
// the reserved set is non-empty), so it's exercised directly here.
func TestSortAndDedupReserved_Empty(t *testing.T) {
	if got := sortAndDedupReserved(nil); got != nil {
		t.Errorf("sortAndDedupReserved(nil) = %v, want nil", got)
	}
	if got := sortAndDedupReserved([]int{}); got != nil {
		t.Errorf("sortAndDedupReserved([]) = %v, want nil", got)
	}
}

// sortAndDedupReserved sorts ascending and removes duplicates so
// the stored Reserved slice is deterministic across runs.
func TestSortAndDedupReserved_SortsAndDedups(t *testing.T) {
	got := sortAndDedupReserved([]int{5, 1, 3, 1, 5, 2})
	want := []int{1, 2, 3, 5}
	if len(got) != len(want) {
		t.Fatalf("len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}
