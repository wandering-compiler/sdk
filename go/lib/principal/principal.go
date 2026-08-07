// Package principal reads the authenticated caller out of a handler's
// context.
//
// On an authenticated request the gateway verifies the credential and
// threads the resulting principal into gRPC metadata that flows into
// every layer:
//
//   - `x-w17-user` — always present: base64(proto-marshaled AuthResp),
//     the full verified principal (user id, permissions, custom claims).
//   - `x-w17-scope-<name>` — one per entry in AuthResp.scopes, present
//     only for the scopes the project actually declares.
//
// This package is that metadata's read side, as [UserID], [Envelope]
// and [Scope], plus its relay side, [ForwardToOutgoing]. It exists
// because the write side (the gateway) and the storage codegen's read
// side already agree on this wire contract, but a hand-written handler
// had no supported way to join in: it had to hardcode the metadata keys
// and hand-roll base64 + proto.Unmarshal, i.e. reimplement platform
// internals. Reach for these helpers instead.
//
// AuthResp is generated per project, so this package cannot name the
// type — the caller passes an instance of their own generated message
// and the helpers fill it in.
//
// Deliberately dependency-light (grpc metadata + protobuf only) so a
// storage / business handler or a plugin module can import it without
// pulling in the gateway's HTTP stack.
//
// # Choosing between these and a data scope
//
// If you want to FILTER rows by the caller, prefer declaring a
// `(w17.db.scope)` on the model: the storage codegen emits the WHERE
// automatically and there is nothing to read. Reach for [UserID] when
// the caller's identity is a VALUE your logic needs — e.g. recording
// the creator of an organization.
package principal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// userMetadataKey is the metadata key the gateway always threads the
// base64-encoded marshaled AuthResp under, whether or not the project
// declares any data scopes.
const userMetadataKey = "x-w17-user"

// scopeKeyPrefix is the metadata key prefix the gateway writes one
// entry per declared scope under: `x-w17-scope-<scope_name>`, the
// scope name verbatim (snake_case; gRPC metadata accepts underscores).
const scopeKeyPrefix = "x-w17-scope-"

// ErrNoPrincipal reports that no verified caller could be read from the
// context. Every failure mode collapses to this — absent metadata, an
// undecodable envelope, an empty user id — because the distinction is
// never actionable for the caller and leaking it tells an unauthenticated
// prober how far it got.
//
// It means the request is unauthenticated from this handler's point of
// view: either it bypassed the gateway (a direct gRPC call), or the RPC
// is `exclude_auth` / its surface declares no `auth_methods`. Handlers
// fail closed on it:
//
//	uid, err := principal.UserID(ctx, &pb.AuthResp{})
//	if err != nil {
//	    return nil, status.Error(codes.Unauthenticated, "authentication required")
//	}
var ErrNoPrincipal = errors.New("principal: no authenticated caller in context")

// UserIDMessage is a project's generated AuthResp — any proto message
// exposing the user id. Generated AuthResp types satisfy this
// structurally; no registration or wiring needed.
type UserIDMessage interface {
	proto.Message
	GetUserId() string
}

// UserID returns the authenticated caller's user id.
//
// dst is an empty instance of the project's generated AuthResp, used as
// the decode target for the envelope fallback:
//
//	uid, err := principal.UserID(ctx, &pb.AuthResp{})
//
// It reads the cheap `x-w17-scope-user_id` key first, then falls back to
// decoding the `x-w17-user` envelope. The fallback is what makes this
// work in a project scoped by something other than user_id (say org_id):
// the gateway only threads the scope key for scopes the project actually
// declares, so the envelope is the only source that is always present.
//
// Fails closed — returns ("", [ErrNoPrincipal]) rather than an empty
// string a caller might mistake for a real id.
func UserID(ctx context.Context, dst UserIDMessage) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ErrNoPrincipal
	}
	if vals := md.Get(ScopeKey("user_id")); len(vals) > 0 && vals[0] != "" {
		return vals[0], nil
	}
	if err := envelopeFromMD(md, dst); err != nil {
		return "", err
	}
	if uid := dst.GetUserId(); uid != "" {
		return uid, nil
	}
	return "", ErrNoPrincipal
}

// Envelope decodes the full verified principal into dst — the project's
// generated AuthResp:
//
//	var who pb.AuthResp
//	if err := principal.Envelope(ctx, &who); err != nil { ... }
//
// Use it when a handler needs more than the user id (permission ids, the
// session token id, custom claims). For just the id, prefer [UserID],
// which also honours the scope fast-path.
//
// Returns [ErrNoPrincipal] if no decodable envelope is present; dst is
// then left as the caller supplied it.
func Envelope(ctx context.Context, dst proto.Message) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ErrNoPrincipal
	}
	return envelopeFromMD(md, dst)
}

// envelopeFromMD is the shared base64 + unmarshal step. Any decode
// failure collapses to ErrNoPrincipal (see the sentinel's doc) while
// staying wrapped so a server-side log can still tell the modes apart.
func envelopeFromMD(md metadata.MD, dst proto.Message) error {
	vals := md.Get(userMetadataKey)
	if len(vals) == 0 || vals[0] == "" {
		return ErrNoPrincipal
	}
	raw, err := base64.StdEncoding.DecodeString(vals[0])
	if err != nil {
		return fmt.Errorf("%w: undecodable envelope: %w", ErrNoPrincipal, err)
	}
	if err := proto.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: unmarshal envelope: %w", ErrNoPrincipal, err)
	}
	return nil
}

// Scope returns the value of a declared data scope — the same
// gateway-stamped metadata the generated storage layer reads for its
// auto-WHERE.
//
// Returns ("", false) when the scope isn't present: the project doesn't
// declare it, or the request is unauthenticated. Fail closed on false —
// an absent scope is never "match everything".
//
//	orgID, ok := principal.Scope(ctx, "org_id")
//	if !ok {
//	    return nil, status.Error(codes.PermissionDenied, "missing required scope: org_id")
//	}
func Scope(ctx context.Context, name string) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(ScopeKey(name))
	if len(vals) == 0 || vals[0] == "" {
		return "", false
	}
	return vals[0], true
}

// ScopeKey returns the canonical gRPC metadata key for a scope name
// (`org_id` → `x-w17-scope-org_id`). Rarely needed directly — prefer
// [Scope]. Exposed so a caller reading metadata by hand spells the key
// the same way the gateway stamps it.
func ScopeKey(name string) string {
	return scopeKeyPrefix + name
}

// ForwardToOutgoing relays the verified principal from a handler's
// INCOMING metadata onto the ctx's OUTGOING metadata, so a hand-written
// tier that dials a downstream tier carries the caller's identity onward.
//
// It is the relay counterpart to the read-side helpers. The gateway stamps
// the principal as gRPC metadata on its first hop, and the doc contract is
// that it "flows into every layer" — but gRPC does NOT copy a server's
// incoming metadata onto the clients it dials. In-process (composed) tiers
// inherit the same ctx so the principal rides along for free; across a real
// gRPC hop it stops at the first tier unless something re-emits it. This is
// that re-emit. The generated inter-tier client installs it automatically
// (see [core/grpcx.DialOpts]); it is exported for the rare hand-built dial
// that bypasses that path.
//
// It moves EXACTLY the principal contract — the `x-w17-user` envelope and
// every `x-w17-scope-<name>` key — and nothing else. Paging (`x-w17-paging-*`),
// tracing, i18n, and tx-routing metadata have their own propagation and are
// deliberately left alone; blanket-forwarding paging in particular would
// bleed one query's LIMIT / keyset onto sibling storage calls.
//
// A key already present in the outgoing metadata is left untouched — an
// explicit value the caller set wins and is never duplicated — so the
// forward is idempotent and safe to apply on every hop. When ctx carries no
// incoming metadata (an unauthenticated / gateway-bypassed call) it is
// returned unchanged, so downstream scope guards still fail closed.
func ForwardToOutgoing(ctx context.Context) context.Context {
	in, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	// grpc-go normalises metadata keys to lowercase on both the incoming and
	// the outgoing side, so the lowercase key constants match verbatim and
	// the already-set check below compares like for like.
	out, _ := metadata.FromOutgoingContext(ctx)
	var pairs []string
	for key, vals := range in {
		if key != userMetadataKey && !strings.HasPrefix(key, scopeKeyPrefix) {
			continue
		}
		if _, set := out[key]; set {
			continue
		}
		for _, v := range vals {
			pairs = append(pairs, key, v)
		}
	}
	if len(pairs) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}
