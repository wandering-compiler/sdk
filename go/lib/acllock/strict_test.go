package acllock_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/acllock"
)

// TestHasAllPermissionsStrict — fails closed on an empty wanted list (denies),
// and otherwise mirrors HasAllPermissions (all-present → true, any-missing →
// false).
func TestHasAllPermissionsStrict(t *testing.T) {
	ids := []int32{1, 2, 3}

	if acllock.HasAllPermissionsStrict(ids) {
		t.Error("empty wanted must fail closed (false)")
	}
	if !acllock.HasAllPermissionsStrict(ids, 1, 2) {
		t.Error("all-present wanted should be true")
	}
	if acllock.HasAllPermissionsStrict(ids, 1, 9) {
		t.Error("any-missing wanted should be false")
	}
}
