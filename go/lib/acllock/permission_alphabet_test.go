package acllock

import "testing"

// T2-5 pass #10 (D-F1). The package documents Lock.Permissions with cascade
// CODE keys ("tasks.Task#add") and describes IDByString as the translation an
// auth backend uses for "typical JWT claim shape". Every lock the compiler
// emits is keyed by enum VALUE NAMES — TASKS_TASK_ADD — because the allocation
// crosses a proto enum, where the code cannot survive.
//
// The two alphabets are disjoint, so the documented call always misses. It
// fails closed (no misgrant), but a legitimate grant is silently dropped, and
// the external author has no way to see why: the code→name conversion lived
// only in the private srcgo tree, so the public SDK described a translation it
// gave no means to perform.
//
// Same shape as pass #9's A-F1, where acl_roles globs were matched against
// enum names while every documented glob was written as a code: not a bug in
// the matcher, a mismatch between the alphabet the docs promise and the one
// the artifact carries.
func TestPermissionKey_ConvertsCodeToTheAlphabetLocksUse(t *testing.T) {
	lock := &Lock{
		Version:     CurrentVersion,
		Permissions: map[string]int{"TASKS_TASK_ADD": 7, "USERS_USERS_SERVICE_SIGN_UP": 9},
	}

	for _, tc := range []struct {
		code string
		want int
	}{
		{"tasks.Task#add", 7},
		{"users.UsersService.SignUp", 9},
	} {
		t.Run(tc.code, func(t *testing.T) {
			got, ok := IDByString(lock, tc.code)
			if !ok {
				t.Fatalf("IDByString(%q) missed — the documented cascade-code form finds nothing in a "+
					"lock keyed by enum value names, which is every lock the compiler emits", tc.code)
			}
			if got != tc.want {
				t.Fatalf("IDByString(%q) = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}

// A key already in the lock's own alphabet must keep resolving directly — the
// conversion is a fallback, not a replacement.
func TestPermissionKey_EnumValueNameStillResolves(t *testing.T) {
	lock := &Lock{Version: CurrentVersion, Permissions: map[string]int{"TASKS_TASK_ADD": 7}}
	if got, ok := IDByString(lock, "TASKS_TASK_ADD"); !ok || got != 7 {
		t.Fatalf("IDByString(name) = (%d,%v), want (7,true)", got, ok)
	}
}

// An unknown permission still misses — the fallback must not invent a match.
func TestPermissionKey_UnknownStillMisses(t *testing.T) {
	lock := &Lock{Version: CurrentVersion, Permissions: map[string]int{"TASKS_TASK_ADD": 7}}
	if _, ok := IDByString(lock, "tasks.Task#delete"); ok {
		t.Fatal("an unallocated permission must not resolve")
	}
}

// PermissionKey is the conversion itself, exported so an auth backend can
// normalise claims once instead of per lookup.
func TestPermissionKey_Conversion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"tasks.Task#view", "TASKS_TASK_VIEW"},
		{"tasks.TaskQuery.ListTasksPaged", "TASKS_TASK_QUERY_LIST_TASKS_PAGED"},
		{"users.UsersService.SignUp", "USERS_USERS_SERVICE_SIGN_UP"},
		{"ALREADY_A_NAME", "ALREADY_A_NAME"},
	} {
		if got := PermissionKey(tc.in); got != tc.want {
			t.Errorf("PermissionKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
