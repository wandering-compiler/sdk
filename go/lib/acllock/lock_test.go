package acllock_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/acllock"
)

// Allocate from empty prev assigns 1..N in declaration order.
func TestAllocate_EmptyPrev(t *testing.T) {
	out := acllock.Allocate(nil, []string{
		"tasks.Task#add",
		"tasks.Task#change",
		"tasks.Task#delete",
	})
	if out.Permissions["tasks.Task#add"] != 1 {
		t.Errorf("add = %d, want 1", out.Permissions["tasks.Task#add"])
	}
	if out.Permissions["tasks.Task#change"] != 2 {
		t.Errorf("change = %d, want 2", out.Permissions["tasks.Task#change"])
	}
	if out.Permissions["tasks.Task#delete"] != 3 {
		t.Errorf("delete = %d, want 3", out.Permissions["tasks.Task#delete"])
	}
	if len(out.Reserved) != 0 {
		t.Errorf("Reserved = %v, want empty", out.Reserved)
	}
}

// Existing perms keep their IDs, new ones get next-free slot.
func TestAllocate_PreservesExisting(t *testing.T) {
	prev := &acllock.Lock{
		Version: acllock.CurrentVersion,
		Permissions: map[string]int{
			"tasks.Task#add":    1,
			"tasks.Task#delete": 3,
		},
		Reserved: []int{2},
	}
	out := acllock.Allocate(prev, []string{
		"tasks.Task#add",
		"tasks.Task#delete",
		"tasks.Task#view", // new
	})
	if out.Permissions["tasks.Task#add"] != 1 {
		t.Errorf("add lost slot: %d", out.Permissions["tasks.Task#add"])
	}
	if out.Permissions["tasks.Task#delete"] != 3 {
		t.Errorf("delete lost slot: %d", out.Permissions["tasks.Task#delete"])
	}
	// new perm picks smallest free (not 1, 2, 3 — all taken or reserved → 4)
	if out.Permissions["tasks.Task#view"] != 4 {
		t.Errorf("view = %d, want 4 (1+3 taken, 2 reserved)", out.Permissions["tasks.Task#view"])
	}
	if !contains(out.Reserved, 2) {
		t.Errorf("reserved should still contain 2, got %v", out.Reserved)
	}
}

// Removed perms move to reserved monotonically.
func TestAllocate_RemovedGoesToReserved(t *testing.T) {
	prev := &acllock.Lock{
		Version: acllock.CurrentVersion,
		Permissions: map[string]int{
			"tasks.Task#add":    1,
			"tasks.Task#delete": 2,
		},
	}
	out := acllock.Allocate(prev, []string{
		"tasks.Task#add", // delete removed
	})
	if out.Permissions["tasks.Task#add"] != 1 {
		t.Errorf("add = %d, want 1", out.Permissions["tasks.Task#add"])
	}
	if _, present := out.Permissions["tasks.Task#delete"]; present {
		t.Errorf("delete should be removed")
	}
	if !contains(out.Reserved, 2) {
		t.Errorf("reserved should contain 2 (delete's old ID), got %v", out.Reserved)
	}
}

// HasPermission scans the granted-IDs slice for `id`.
func TestHasPermission(t *testing.T) {
	ids := []int32{1, 3, 9, 17}
	cases := []struct {
		id   int32
		want bool
	}{
		{0, false}, // 0 not a valid ID
		{1, true},
		{2, false},
		{3, true},
		{8, false},
		{9, true},
		{16, false},
		{17, true},
		{18, false},
		{100, false}, // not in granted list
	}
	for _, c := range cases {
		if got := acllock.HasPermission(ids, c.id); got != c.want {
			t.Errorf("id=%d → %v, want %v", c.id, got, c.want)
		}
	}
}

// HasAnyPermission short-circuits on first match; empty input
// is false-by-default.
func TestHasAnyPermission(t *testing.T) {
	ids := []int32{1, 5}
	cases := []struct {
		name   string
		wanted []int32
		want   bool
	}{
		{"single match", []int32{5}, true},
		{"first matches", []int32{1, 99}, true},
		{"second matches", []int32{99, 5}, true},
		{"no match", []int32{2, 3, 4}, false},
		{"empty input is false", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := acllock.HasAnyPermission(ids, c.wanted...)
			if got != c.want {
				t.Errorf("wanted=%v → %v, want %v", c.wanted, got, c.want)
			}
		})
	}
}

// HasAllPermissions short-circuits on first miss; empty input
// is true-by-default (vacuously satisfied).
func TestHasAllPermissions(t *testing.T) {
	ids := []int32{1, 5, 7}
	cases := []struct {
		name   string
		wanted []int32
		want   bool
	}{
		{"all match", []int32{1, 5, 7}, true},
		{"first misses", []int32{99, 5}, false},
		{"last misses", []int32{1, 5, 99}, false},
		{"single match", []int32{5}, true},
		{"single miss", []int32{99}, false},
		{"empty input is true", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := acllock.HasAllPermissions(ids, c.wanted...)
			if got != c.want {
				t.Errorf("wanted=%v → %v, want %v", c.wanted, got, c.want)
			}
		})
	}
}

// GrantAll covers every allocated ID but not reserved IDs.
func TestGrantAll(t *testing.T) {
	lock := &acllock.Lock{
		Version: acllock.CurrentVersion,
		Permissions: map[string]int{
			"a": 1,
			"b": 3,
		},
		Reserved: []int{2}, // formerly used, now retired
	}
	ids := acllock.GrantAll(lock)
	if !acllock.HasPermission(ids, 1) {
		t.Error("a (id 1) should be granted")
	}
	if !acllock.HasPermission(ids, 3) {
		t.Error("b (id 3) should be granted")
	}
	if acllock.HasPermission(ids, 2) {
		t.Error("reserved id 2 should NOT be granted by GrantAll")
	}
}

// IDByString / StringByID round-trip.
func TestIDStringLookups(t *testing.T) {
	lock := &acllock.Lock{
		Version:     acllock.CurrentVersion,
		Permissions: map[string]int{"tasks.Task#add": 1, "users.User#view": 5},
	}
	if id, ok := acllock.IDByString(lock, "tasks.Task#add"); !ok || id != 1 {
		t.Errorf("IDByString(tasks.Task#add) = (%d, %v), want (1, true)", id, ok)
	}
	if _, ok := acllock.IDByString(lock, "ghost"); ok {
		t.Error("IDByString on unknown should return ok=false")
	}
	if s, ok := acllock.StringByID(lock, 5); !ok || s != "users.User#view" {
		t.Errorf("StringByID(5) = (%q, %v), want (users.User#view, true)", s, ok)
	}
	if _, ok := acllock.StringByID(lock, 99); ok {
		t.Error("StringByID on unallocated should return ok=false")
	}
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
