package acllock_test

import (
	"slices"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/acllock"
)

func TestPermissionsOfRoles(t *testing.T) {
	roles := map[string][]int32{
		"advisor": {4, 2, 7},
		"viewer":  {2},
		"admin":   {1, 2, 3, 4, 5, 6, 7},
	}
	for _, tc := range []struct {
		name string
		keys []string
		want []int32
	}{
		{"one role", []string{"viewer"}, []int32{2}},
		{"union, sorted, deduped", []string{"advisor", "viewer"}, []int32{2, 4, 7}},
		{"a key this domain does not declare carries nothing", []string{"retired", "viewer"}, []int32{2}},
		{"no keys", nil, nil},
		{"only unknown keys", []string{"retired"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := acllock.PermissionsOfRoles(roles, tc.keys); !slices.Equal(got, tc.want) {
				t.Fatalf("PermissionsOfRoles(%v) = %v, want %v", tc.keys, got, tc.want)
			}
		})
	}
}
