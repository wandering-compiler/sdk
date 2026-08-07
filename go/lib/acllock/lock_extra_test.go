package acllock_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/acllock"
)

// Clone on a nil receiver yields nil rather than panicking — the
// deep-copy helper must tolerate the zero pointer.
func TestClone_Nil(t *testing.T) {
	var l *acllock.Lock
	if got := l.Clone(); got != nil {
		t.Errorf("(*Lock)(nil).Clone() = %v, want nil", got)
	}
}

// Clone produces an independent deep copy: mutating the clone's
// Permissions / Reserved must not bleed back into the original.
func TestClone_DeepCopyIsolated(t *testing.T) {
	orig := &acllock.Lock{
		Version:     acllock.CurrentVersion,
		Permissions: map[string]int{"a": 1, "b": 2},
		Reserved:    []int{3, 4},
		Checksum:    "sha256:deadbeef",
	}
	clone := orig.Clone()
	if clone == orig {
		t.Fatal("Clone returned the same pointer")
	}
	// Field-level equality.
	if clone.Version != orig.Version || clone.Checksum != orig.Checksum {
		t.Errorf("scalar fields not copied: %+v", clone)
	}
	if clone.Permissions["a"] != 1 || clone.Permissions["b"] != 2 {
		t.Errorf("Permissions not copied: %v", clone.Permissions)
	}
	if !contains(clone.Reserved, 3) || !contains(clone.Reserved, 4) {
		t.Errorf("Reserved not copied: %v", clone.Reserved)
	}
	// Mutate the clone; original must be untouched.
	clone.Permissions["a"] = 99
	clone.Reserved = append(clone.Reserved, 5)
	if orig.Permissions["a"] != 1 {
		t.Errorf("clone mutation bled into original Permissions: %v", orig.Permissions)
	}
	if contains(orig.Reserved, 5) {
		t.Errorf("clone Reserved append bled into original: %v", orig.Reserved)
	}
}

// MaxID returns the largest ID across both Permissions and
// Reserved; nil receiver and empty lock both yield 0.
func TestMaxID(t *testing.T) {
	var nilLock *acllock.Lock
	if got := nilLock.MaxID(); got != 0 {
		t.Errorf("nil lock MaxID = %d, want 0", got)
	}
	if got := (&acllock.Lock{}).MaxID(); got != 0 {
		t.Errorf("empty lock MaxID = %d, want 0", got)
	}
	// Largest may live in Reserved rather than Permissions.
	l := &acllock.Lock{
		Permissions: map[string]int{"a": 2, "b": 5},
		Reserved:    []int{9, 1},
	}
	if got := l.MaxID(); got != 9 {
		t.Errorf("MaxID = %d, want 9 (from Reserved)", got)
	}
	// Largest may live in Permissions.
	l2 := &acllock.Lock{
		Permissions: map[string]int{"a": 12},
		Reserved:    []int{3},
	}
	if got := l2.MaxID(); got != 12 {
		t.Errorf("MaxID = %d, want 12 (from Permissions)", got)
	}
}

// Allocate skips empty permission strings rather than assigning
// them an ID — a stray "" in the current set must not consume a slot.
func TestAllocate_SkipsEmptyPermString(t *testing.T) {
	out := acllock.Allocate(nil, []string{"", "real.Perm#do", ""})
	if _, present := out.Permissions[""]; present {
		t.Error("empty perm string should not be allocated an ID")
	}
	if out.Permissions["real.Perm#do"] != 1 {
		t.Errorf("real perm = %d, want 1 (empty strings consume no slot)", out.Permissions["real.Perm#do"])
	}
	if len(out.Permissions) != 1 {
		t.Errorf("Permissions = %v, want a single entry", out.Permissions)
	}
}

// GrantAll on a nil lock and on a lock with no permissions both
// return nil — the "grant everything" surface of an empty lock is
// the empty grant.
func TestGrantAll_NilAndEmpty(t *testing.T) {
	if got := acllock.GrantAll(nil); got != nil {
		t.Errorf("GrantAll(nil) = %v, want nil", got)
	}
	empty := &acllock.Lock{Version: acllock.CurrentVersion, Permissions: map[string]int{}}
	if got := acllock.GrantAll(empty); got != nil {
		t.Errorf("GrantAll(empty) = %v, want nil", got)
	}
}

// IDByString / StringByID on a nil lock return the not-found
// sentinel rather than dereferencing a nil pointer.
func TestLookups_NilLock(t *testing.T) {
	if id, ok := acllock.IDByString(nil, "anything"); ok || id != 0 {
		t.Errorf("IDByString(nil) = (%d, %v), want (0, false)", id, ok)
	}
	if s, ok := acllock.StringByID(nil, 1); ok || s != "" {
		t.Errorf("StringByID(nil) = (%q, %v), want (\"\", false)", s, ok)
	}
}
