package acllock

import "testing"

func TestHeldInAnOrganization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes map[string]string
		id     int32
		want   bool
	}{
		{"no scopes", nil, 7, false},
		{"no hint", map[string]string{"org_id": "x"}, 7, false},
		{"listed", map[string]string{OrgPermissionsScope: "3,7,12"}, 7, true},
		{"not listed", map[string]string{OrgPermissionsScope: "3,12"}, 7, false},
		// A prefix of another id is not that id.
		{"prefix only", map[string]string{OrgPermissionsScope: "17,70"}, 7, false},
		{"wildcard", map[string]string{OrgPermissionsScope: "*"}, 7, true},
		{"malformed", map[string]string{OrgPermissionsScope: "seven"}, 7, false},
		// The product's codes are not ids until its gateway translates them.
		{"untranslated product codes", map[string]string{OrgProductRolesScope: "7"}, 7, false},
	} {
		if got := HeldInAnOrganization(tc.scopes, tc.id); got != tc.want {
			t.Errorf("%s: HeldInAnOrganization(%v, %d) = %v, want %v", tc.name, tc.scopes, tc.id, got, tc.want)
		}
	}
}

func TestFormatOrgPermissions_SortedDeduplicated(t *testing.T) {
	if got := FormatOrgPermissions([]int32{12, 3, 12, 7}); got != "3,7,12" {
		t.Errorf("FormatOrgPermissions = %q, want 3,7,12", got)
	}
	if got := FormatOrgPermissions(nil); got != "" {
		t.Errorf("FormatOrgPermissions(nil) = %q, want empty", got)
	}
}

func TestOrgPermissionsOfProductRoles(t *testing.T) {
	roles := map[string][]int32{"notes.write": {4}, "notes.read": {2, 3}}

	// The identity's own ids are dropped: against the product's lock they would
	// name unrelated permissions.
	scopes := map[string]string{
		"org_id":             "",
		OrgPermissionsScope:  "2",
		OrgProductRolesScope: "notes.write, notes.read,unknown",
	}
	got := OrgPermissionsOfProductRoles(scopes, roles)
	if got[OrgPermissionsScope] != "2,3,4" {
		t.Errorf("translated hint = %q, want 2,3,4", got[OrgPermissionsScope])
	}
	if _, ok := got[OrgProductRolesScope]; ok {
		t.Error("the codes stayed in the scopes after translation")
	}

	scopes = map[string]string{OrgPermissionsScope: "9"}
	if got := OrgPermissionsOfProductRoles(scopes, roles); got[OrgPermissionsScope] != "" {
		t.Errorf("an identity hint with no product codes survived: %q", got[OrgPermissionsScope])
	}
	scopes = map[string]string{OrgProductRolesScope: "unknown"}
	if got := OrgPermissionsOfProductRoles(scopes, roles); got[OrgPermissionsScope] != "" {
		t.Errorf("codes the product does not know produced a hint: %q", got[OrgPermissionsScope])
	}
	if OrgPermissionsOfProductRoles(nil, roles) != nil {
		t.Error("nil scopes did not stay nil")
	}
}
