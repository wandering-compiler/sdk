package acllock_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/acllock"
)

// assertNoLostIDs checks that every ID from 1 to MaxID is either assigned
// to exactly one permission or reserved, never both and never neither.
func assertNoLostIDs(t *testing.T, l *acllock.Lock) {
	t.Helper()
	owner := map[int]string{}
	for perm, id := range l.Permissions {
		if other, dup := owner[id]; dup {
			t.Errorf("ID %d assigned to both %q and %q", id, other, perm)
		}
		owner[id] = perm
	}
	reserved := map[int]bool{}
	for _, id := range l.Reserved {
		if _, assigned := owner[id]; assigned {
			t.Errorf("ID %d is both assigned (%q) and reserved", id, owner[id])
		}
		reserved[id] = true
	}
	for id := 1; id <= l.MaxID(); id++ {
		if _, assigned := owner[id]; !assigned && !reserved[id] {
			t.Errorf("ID %d consumed but neither assigned nor reserved (lock %+v)", id, l)
		}
	}
}

// INVARIANT: a permission listed twice gets one ID, and no ID is spent on
// the repeat.
func TestAllocate_DuplicateEntryConsumesNoID(t *testing.T) {
	got := acllock.Allocate(nil, []string{"a", "b", "a", "", "c", "c"})
	want := map[string]int{"a": 1, "b": 2, "c": 3}
	if len(got.Permissions) != len(want) {
		t.Fatalf("Permissions = %v, want %v", got.Permissions, want)
	}
	for perm, id := range want {
		if got.Permissions[perm] != id {
			t.Errorf("%s = %d, want %d (full: %v)", perm, got.Permissions[perm], id, got.Permissions)
		}
	}
	if len(got.Reserved) != 0 {
		t.Errorf("Reserved = %v, want none", got.Reserved)
	}
	assertNoLostIDs(t, got)

	// A duplicate of an existing permission keeps its ID too.
	prev := &acllock.Lock{Version: acllock.CurrentVersion, Permissions: map[string]int{"a": 1}}
	got = acllock.Allocate(prev, []string{"a", "a", "b", "b"})
	if got.Permissions["a"] != 1 || got.Permissions["b"] != 2 {
		t.Errorf("Permissions = %v, want a=1 b=2", got.Permissions)
	}
	assertNoLostIDs(t, got)
}

// INVARIANT: across generations — with additions, removals, re-adds and
// duplicates — every ID stays accounted for, Reserved only grows, and a
// reserved ID is never handed out again.
func TestAllocate_GenerationsKeepEveryIDAccountedFor(t *testing.T) {
	generations := [][]string{
		{"a", "b", "c"},
		{"a", "c", "d", "d"},      // b removed, d added twice
		{"c", "e", "b", "e"},      // a, d removed; b re-added as new
		{"c", "e", "b", "f", "c"}, // f added, c repeated
		{"f"},                     // most removed
		{"g", "a", "g", "b", "a"}, // old names re-added, duplicated
	}
	var l *acllock.Lock
	everReserved := map[int]bool{}
	for i, current := range generations {
		l = acllock.Allocate(l, current)
		assertNoLostIDs(t, l)
		for id := range everReserved {
			if !containsID(l.Reserved, id) {
				t.Errorf("generation %d: reserved ID %d left Reserved", i, id)
			}
		}
		for perm, id := range l.Permissions {
			if everReserved[id] {
				t.Errorf("generation %d: reserved ID %d reused for %q", i, id, perm)
			}
		}
		for _, id := range l.Reserved {
			everReserved[id] = true
		}
	}
}

func containsID(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
