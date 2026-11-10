package acllock_test

import (
	"reflect"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/acllock"
)

// IDs are allocated from 1: 0 (and below) names no permission. A granted
// list that carries a 0 — an unset proto3 int32 slot, a zero-valued
// permission constant — must not satisfy a check for 0.
func TestHasPermission_IDZeroOrBelowIsNeverGranted(t *testing.T) {
	ids := []int32{0, 3, -1}
	if acllock.HasPermission(ids, 0) {
		t.Error("HasPermission(ids with 0, 0) = true; 0 names no permission")
	}
	if acllock.HasPermission(ids, -1) {
		t.Error("HasPermission(ids with -1, -1) = true; a negative id names no permission")
	}
	if !acllock.HasPermission(ids, 3) {
		t.Error("HasPermission(ids, 3) = false")
	}
	if acllock.HasAnyPermission(ids, 0) || acllock.HasAllPermissionsStrict(ids, 0) || acllock.HasAllPermissions(ids, 0, 3) {
		t.Error("a check for id 0 granted through HasAny/HasAll/HasAllStrict")
	}
}

// Allocate is deterministic: the same previous lock and the same current
// list give the same lock, whatever order the previous lock's map is walked
// in (Go randomises it on every range).
func TestAllocate_IsDeterministic(t *testing.T) {
	prev := func() *acllock.Lock {
		return &acllock.Lock{Version: acllock.CurrentVersion, Permissions: map[string]int{
			"a#add": 1, "a#change": 2, "a#delete": 3, "b#add": 5, "b#change": 8, "c#gone": 4, "d#gone": 7,
		}, Reserved: []int{6}}
	}
	current := []string{"b#change", "z#new", "a#add", "y#new", "a#delete", "x#new", "b#add"}
	first := acllock.Allocate(prev(), current)
	for range 200 {
		if got := acllock.Allocate(prev(), current); !reflect.DeepEqual(got, first) {
			t.Fatalf("Allocate is not deterministic:\n%+v\n%+v", first, got)
		}
	}
}
