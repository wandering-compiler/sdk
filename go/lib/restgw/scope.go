// Data-scope runtime helpers (REV-147). The gateway router
// extracts each entry from `AuthResp.scopes` after Authenticate
// succeeds and threads it into outgoing gRPC metadata under
// `ScopeKey(name)`. Storage handlers then read the value back
// via `metadata.FromIncomingContext(...).Get(ScopeKey(name))`
// when generating the auto-WHERE filter for queries against
// scoped models.
//
// Two functions only — the rest of the scope machinery lives
// in storage codegen + gateway codegen. This file is the
// HTTP-level surface so the metadata-key convention has a
// single canonical owner.

package restgw

import (
	"net/http"
)

// scopeKeyPrefix is the gRPC metadata key prefix the gateway
// writes per scope. Matches the REV-147 spec contract:
// `x-w17-scope-<scope_name>` with the scope name preserved
// verbatim (snake_case from the annotation; gRPC metadata
// accepts underscores).
const scopeKeyPrefix = "x-w17-scope-"

// ScopeKey returns the canonical gRPC metadata key for a
// given scope name. Used by:
//
//   - Gateway router emit: per-request,
//     `metadata.AppendToOutgoingContext(ctx, ScopeKey("tenant_id"), v)`
//     for each entry in AuthResp.scopes.
//   - Storage handler emit: per-request,
//     `metadata.FromIncomingContext(ctx).Get(ScopeKey("tenant_id"))`
//     to read the scope value before binding it into the
//     generated WHERE / SET clause.
//
// Both sides use the same function so the wire-format
// contract has exactly one source of truth.
//
// `name` is the scope's annotation name (e.g. `"tenant_id"`).
// Caller is responsible for passing a snake_case identifier;
// the parser validates the annotation upstream so this
// function doesn't re-check.
func ScopeKey(name string) string {
	return scopeKeyPrefix + name
}

// WriteMissingScope writes the canonical 403 envelope for
// the REV-147 fail-closed runtime check. Generated storage
// handlers call this when a required scope's metadata key
// isn't present on the incoming gRPC context — i.e. the
// caller authenticated but the auth backend didn't populate
// `AuthResp.scopes["<name>"]` (or the gateway router didn't
// thread it).
//
// Message format pinned per spec: `"missing required scope:
// <name>"`. Clients can branch on the suffix to surface a
// user-friendly "you don't have access to any <tenant>"
// message; the canonical wire shape stays stable.
//
// This is a thin specialization over `WriteForbidden(w,
// reason)` so the error envelope shape stays consistent
// across the REV-146 permission check + REV-147 scope check
// (same code, same status, just a different message).
func WriteMissingScope(w http.ResponseWriter, name string) {
	WriteError(w, http.StatusForbidden, "PERMISSION_DENIED",
		"missing required scope: "+name)
}
