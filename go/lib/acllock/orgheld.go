package acllock

import (
	"slices"
	"strconv"
	"strings"
)

// A request that names no organization, from a caller who holds roles INSIDE
// organizations, is narrowed to the caller's realm-wide roles: the identity
// cannot know which company the request meant, and guessing would decide whose
// data a write lands in. Refusing that request as MISSING_PERMISSION, though,
// tells the caller "you have no access" when the truth is "say where" — the
// product cannot ask which organization to act in, and sends a person to an
// administrator over roles that are fine.
//
// So the identity says what was set aside, and the gateway's gate reads it
// when a permission is missing:
//
//   - OrgPermissionsScope — the permission ids (sorted, comma-separated, or
//     `*` for a wildcard role) the caller holds through roles that apply only
//     inside an organization the request did not name. Written by the identity
//     for its own domain's ids.
//   - OrgProductRolesScope — the same, for a PRODUCT's gateway: the product's
//     permission codes (comma-separated). The identity cannot know a
//     product's ids, so the product's gateway translates them into
//     OrgPermissionsScope (OrgPermissionsOfProductRoles) next to the codes
//     it already translates for the permission set itself.
//
// Both are scopes because scopes are the one map every auth response that a
// gate reads already declares, so no project's AuthResp type has to grow a
// field. They name only what the caller holds — never which organizations, nor
// any organization the caller is not a member of — and they are written only
// when the request could have named one (the W17-Org header is accepted
// there) and did not.
const (
	OrgPermissionsScope  = "org_permissions"
	OrgProductRolesScope = "org_product_roles"
)

// HeldInAnOrganization reports whether the caller holds permission `id` inside
// an organization the request did not name — the case the gate answers with
// ORGANIZATION_REQUIRED instead of MISSING_PERMISSION.
//
// A missing or malformed scope answers false, which keeps the refusal the
// MISSING_PERMISSION it always was: the hint can only make a refusal more
// precise, never turn one into an admission.
func HeldInAnOrganization(scopes map[string]string, id int32) bool {
	v := scopes[OrgPermissionsScope]
	if v == "" {
		return false
	}
	if v == "*" {
		return true
	}
	want := strconv.Itoa(int(id))
	for _, s := range strings.Split(v, ",") {
		if strings.TrimSpace(s) == want {
			return true
		}
	}
	return false
}

// FormatOrgPermissions renders a permission set the way OrgPermissionsScope
// carries it: sorted, deduplicated, comma-separated; "" for none.
func FormatOrgPermissions(ids []int32) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = strconv.Itoa(int(id))
	}
	return strings.Join(parts, ",")
}

// OrgPermissionsOfProductRoles is what a product's gateway does to the hint
// the identity answered it: the product's permission codes held inside
// organizations (OrgProductRolesScope) become this product's permission ids
// (OrgPermissionsScope), through the same table that translates the caller's
// permission set (PermissionsOfRoles).
//
// Whatever the identity put under OrgPermissionsScope is dropped first: those
// are the IDENTITY's ids, and read against a product's lock they would name
// unrelated permissions. Returns the map it was given, changed in place (nil
// stays nil).
func OrgPermissionsOfProductRoles(scopes map[string]string, roles map[string][]int32) map[string]string {
	if scopes == nil {
		return nil
	}
	codes := scopes[OrgProductRolesScope]
	delete(scopes, OrgProductRolesScope)
	delete(scopes, OrgPermissionsScope)
	if codes == "" {
		return scopes
	}
	var keys []string
	for _, c := range strings.Split(codes, ",") {
		if c = strings.TrimSpace(c); c != "" {
			keys = append(keys, c)
		}
	}
	if ids := FormatOrgPermissions(PermissionsOfRoles(roles, keys)); ids != "" {
		scopes[OrgPermissionsScope] = ids
	}
	return scopes
}
