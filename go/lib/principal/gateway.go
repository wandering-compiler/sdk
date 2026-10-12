package principal

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// LabelKeyPrefix is the metadata prefix carrying an event's BROADCAST
// labels — `w17-label-<name>`, one per entry in the verified principal's
// AuthResp.labels. The /w17-events hub matches an event's labels against
// each connected principal's before delivering, so this namespace decides
// who receives an event, exactly as [ScopeKey] decides which rows a
// request may touch.
const LabelKeyPrefix = "w17-label-"

// gatewayOwnedPrefixes are the metadata namespaces a public gateway OWNS:
// it writes them from the verified auth response and every downstream tier
// trusts them. A client must never be able to put a value in one.
//
// The prefixes are exact on purpose. `w17-org` is a legitimate CLIENT
// header — the caller's active organization, which the console validates
// against membership before trusting — so stripping `w17-` wholesale would
// break it. This is the same rule the gateway parser enforces at
// declaration time for `metadata_bindings` / `metadata_propagation`; this
// is its wire-side half.
var gatewayOwnedPrefixes = []string{"x-w17-", LabelKeyPrefix}

// gatewayOwnedKeys are the exact keys a client must never set beside the
// prefixes: the transaction a storage call adopts (txregistry.HeaderName)
// and the connection affinity it is routed by (distx.ConnIDHeader). Their
// only legitimate writer is the transaction coordinator inside the bundle;
// one forwarded from a client would make a storage call join another
// request's open transaction, or pin its routing. Matched on the whole
// name, so `w17-tx-id-hint` is an ordinary key.
//
// Literals, for the reason lib/eventbus keeps one: lib/ does not import
// service/. service/tx pins both against [IsGatewayOwnedKey] in its tests.
var gatewayOwnedKeys = []string{"w17-tx-id", "w17-conn-id"}

// SanitizeGatewayMD returns a copy of ctx's INCOMING metadata with the
// gateway-owned keys ([IsGatewayOwnedKey]) removed, ready for the caller to
// stamp its verified principal onto and hand to [WithGatewayMD].
//
// Clearing rather than overwriting is the point. A gateway stamps
// `x-w17-scope-<name>` once per scope the auth response actually returns,
// so any scope it OMITS would keep the client's value — and omission is
// normal, not exotic: a scope decorator that cannot resolve an unambiguous
// value deliberately stamps nothing so the scoped model fails closed.
// Without the strip that fail-closed branch is the forgeable one. The
// unauthenticated (`exclude_auth`) path needs it just as much, since
// nothing overwrites anything there at all.
//
// Never returns nil: a request with no metadata yields an empty MD the
// caller can Set into.
func SanitizeGatewayMD(ctx context.Context) metadata.MD {
	in, _ := metadata.FromIncomingContext(ctx)
	md := in.Copy()
	if md == nil {
		md = metadata.MD{}
	}
	for k := range md {
		if IsGatewayOwnedKey(k) {
			delete(md, k)
		}
	}
	return md
}

// WithGatewayMD installs the gateway's sanitized, principal-stamped
// metadata on BOTH sides of ctx — outgoing, which the backend clients
// read, and incoming, which everything that treats ctx as "the request I
// am serving" reads.
//
// Both, because sanitizing one side of a context is not sanitizing the
// context. The strip used to produce an outgoing-only context and leave
// the raw incoming metadata in place, which reads as safe right up until
// something downstream consults the other side — and something does: the
// gateway-tier event emit snapshots the request's metadata onto
// `Event.Metadata` and its `w17-label-*` entries onto `Event.Labels`.
// A caller could therefore choose which tenant's live stream its events
// landed in, and ride a forged `x-w17-scope-*` through the subscriber's
// reinjected context into a storage auto-WHERE — a cross-tenant read or
// write performed by a trusted intermediary on forged input. The backend
// forward call was never the exposed part; the event path was.
//
// The two sides get independent copies so a later Set through one handle
// cannot rewrite the other.
func WithGatewayMD(ctx context.Context, md metadata.MD) context.Context {
	ctx = metadata.NewIncomingContext(ctx, md.Copy())
	return metadata.NewOutgoingContext(ctx, md)
}
