package acllock

// HasPermission reports whether `id` is present in the granted
// permission slice `ids`. Linear scan — n is small (typically
// the union of permissions across a user's roles, dozens at
// most). Out-of-range / not-granted IDs return false.
//
// Used by generated gateway handlers as
// `acllock.HasPermission(authResp.GetPermissionIds(), <id>)`
// before gRPC dispatch. Generator bakes the literal int ID
// resolved from the per-domain acl.lock.json at codegen time.
//
// Wire model: `AuthResp.permission_ids` carries the deduped
// list of granted permission numeric IDs. No bitset packing —
// the slice IS the wire shape (see
// docs/specs/plugins/auth.md §"Permission wire format").
func HasPermission(ids []int32, id int32) bool {
	for _, granted := range ids {
		if granted == id {
			return true
		}
	}
	return false
}

// HasAnyPermission returns true when AT LEAST ONE id in `wanted`
// is present in `ids` (OR semantic). Short-circuits on the first
// match. Empty `wanted` returns false (defensive — a check
// against the empty set is meaningless; callers should not
// invoke without IDs, but if they do the safe default is
// "no permission granted").
//
// Typical use in hand-written handler bodies: gating on
// "caller has at least one of these admin-role perms"
// without writing the per-id check explicitly:
//
//	if !acllock.HasAnyPermission(ids, int32(appacl.AdminUsers), int32(appacl.AdminAudit)) { ... }
func HasAnyPermission(ids []int32, wanted ...int32) bool {
	for _, id := range wanted {
		if HasPermission(ids, id) {
			return true
		}
	}
	return false
}

// HasAllPermissions returns true when EVERY id in `wanted` is
// present in `ids` (AND semantic). Short-circuits on the first
// miss. Empty `wanted` returns true (a check against the empty
// set is vacuously satisfied).
//
// ⚠ SECURITY (acl-sec-3): the empty-`wanted` → true semantic is a
// footgun for authorization gates that build `wanted` from runtime
// data (a config/route map): a path that yields an EMPTY list grants
// access to a principal with no matching permission. For an
// authorization gate fed a runtime-derived list, use
// [HasAllPermissionsStrict], which denies on empty. Keep this combinator
// only for compile-time-fixed permission lists.
//
// Typical use: gating on "caller has BOTH the read AND the
// write perm for a specific entity" without two sequential
// HasPermission calls. The gateway codegen's own per-handler
// emit prefers two sequential checks (one per call site)
// because the two perms have different error-context lines;
// hand-written handler bodies that want a single
// authorization gate prefer this combinator.
func HasAllPermissions(ids []int32, wanted ...int32) bool {
	for _, id := range wanted {
		if !HasPermission(ids, id) {
			return false
		}
	}
	return true
}

// HasAllPermissionsStrict is [HasAllPermissions] but FAILS CLOSED on an
// empty `wanted` (returns false). Use it for authorization gates whose
// required-permission list is built from runtime data, so an
// accidentally-empty list denies rather than grants.
func HasAllPermissionsStrict(ids []int32, wanted ...int32) bool {
	if len(wanted) == 0 {
		return false
	}
	return HasAllPermissions(ids, wanted...)
}

// GrantAll returns the list of every allocated permission ID in
// `lock` — the "default = grant everything" auth backend shape.
// Operator wraps it in their Authenticate handler and every
// request gets the full set.
//
//	func (s *Server) Authenticate(_ context.Context, _ *AuthReq) (*AuthResp, error) {
//	    return &AuthResp{PermissionIds: acllock.GrantAll(aclLock)}, nil
//	}
//
// Reserved IDs (formerly allocated, now retired) are NOT
// included — the auth backend's "grant all" reflects the
// current permission surface, not the historical one.
func GrantAll(lock *Lock) []int32 {
	if lock == nil || len(lock.Permissions) == 0 {
		return nil
	}
	out := make([]int32, 0, len(lock.Permissions))
	for _, id := range lock.Permissions {
		out = append(out, int32(id))
	}
	return out
}

// IDByString returns the allocated ID for a permission string,
// or (0, false) when the string isn't in the lock. Used by
// auth backends that consume permissions as strings (typical
// JWT claim shape) and need to translate to IDs before
// populating AuthResp.permission_ids.
func IDByString(lock *Lock, perm string) (int, bool) {
	if lock == nil {
		return 0, false
	}
	id, ok := lock.Permissions[perm]
	return id, ok
}

// StringByID returns the canonical permission string for an
// allocated ID. Reverse of IDByString. Used by admin tooling /
// audit log / debug — the runtime check path operates on IDs
// directly. Returns ("", false) for unallocated IDs.
//
// Cost: O(n) over Permissions. Cache the reverse map at boot
// when the call site is hot (admin UI populating a "perm
// catalog" dropdown, for example).
func StringByID(lock *Lock, id int) (string, bool) {
	if lock == nil {
		return "", false
	}
	for perm, v := range lock.Permissions {
		if v == id {
			return perm, true
		}
	}
	return "", false
}
