// Data-scope runtime helpers. The gateway router
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
	"context"
	"net/http"

	"google.golang.org/grpc/codes"
)

// scopeKeyPrefix is the gRPC metadata key prefix the gateway
// writes per scope. Matches the data-scope contract:
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

// WriteMissingScope writes the canonical 403 envelope for the fail-closed
// data-scope runtime check. Generated storage handlers call this when a
// required scope's metadata key isn't present on the incoming gRPC context —
// i.e. the caller authenticated but the auth backend didn't populate
// `AuthResp.scopes["<name>"]` (or the gateway router didn't thread it).
//
// It is [WriteMissingScopeCtx] without a request in scope: the sentence stays
// in the source locale and the report carries no trace.
func WriteMissingScope(w http.ResponseWriter, name string) {
	WriteMissingScopeCtx(context.Background(), w, name)
}

// WriteMissingScopeCtx is the two-audience form of the missing-scope refusal.
//
// The scope's name is configuration — which column this deployment isolates
// tenants on — and a caller can do nothing with it, so it goes to the
// operator only. The client gets PERMISSION_DENIED and the code's catalog
// sentence, translated with the request's locale, exactly like any other
// refusal of access.
func WriteMissingScopeCtx(ctx context.Context, w http.ResponseWriter, name string) {
	WriteRefusal(ctx, w, codes.PermissionDenied, "missing required scope: "+name)
}
