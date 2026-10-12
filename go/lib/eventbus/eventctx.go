package eventbus

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// LabelKeyPrefix is the gRPC metadata prefix carrying an event's
// BROADCAST labels: `w17-label-<name>`. The gateway stamps one entry per
// entry in the verified principal's `AuthResp.labels`, and the /w17-events
// hub matches an event's labels against each connected principal's before
// delivering.
//
// Re-exported, not redeclared: the namespace is OWNED by the gateway side
// ([principal.LabelKeyPrefix]), which also strips it off a client's
// metadata. Two spellings of a reserved prefix are two chances to disagree
// about what is reserved.
const LabelKeyPrefix = principal.LabelKeyPrefix

// contextMD picks THE ONE metadata side an event's context data is read
// from, for every emit tier and for the reserved invalidate topic alike.
//
// Incoming first, outgoing second, and never a merge of the two:
//
//   - a SERVER-tier emit fires inside a handler that RECEIVED the call, so
//     the gateway's stamps (x-w17-user, x-w17-scope-*, w17-label-*, trace
//     context) arrived as INCOMING metadata;
//   - a GATEWAY-tier emit fires from the handler that just BUILT the
//     outgoing context it is about to forward on, so nothing has arrived
//     and the same stamps are OUTGOING.
//
// Reading a different side per field is what made these two diverge: the
// labels reader was fixed to consult both sides and the metadata reader
// beside it was not, so every REST/MCP gateway-tier event shipped a
// labelled envelope with an EMPTY Metadata map — and subscriber dispatch
// reinjects exactly that map to carry `x-w17-scope-*` into the handler
// call, so a subscriber on such an event was denied by the scope guard
// three services from the cause. One chooser means the envelope's fields
// can no longer describe two different requests.
//
// Not a merge, deliberately: on the rpc tier both sides are populated and
// only one of them is the sanitized copy the gateway stamped its verified
// principal onto (the interceptor installs it on BOTH sides for exactly
// this reason — see principal.WithGatewayMD). Merging would let whichever
// side is not that copy contribute keys.
func contextMD(ctx context.Context) (metadata.MD, bool) {
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md) > 0 {
		return md, true
	}
	md, ok := metadata.FromOutgoingContext(ctx)
	return md, ok && len(md) > 0
}

// MetadataFromContext snapshots the originating RPC's gRPC metadata into a
// flat string→string map for `Event.Metadata`.
//
// What is carried: everything an event legitimately needs on the far side —
// the gateway-stamped principal and scopes (`x-w17-*`), broadcast labels
// (`w17-label-*`), locale (`w17-language`, `w17-format-locale`), trace and
// correlation context (`traceparent`, `tracestate`, `baggage`,
// `x-request-id`) and any custom header the API declares for propagation.
//
// What is not: transport-internal keys and credentials ([ReservedMetadataKey]).
// The map is persisted with the event — in the broker stream and in the
// dead-letter stream, possibly for a long time — and replayed into every
// subscriber's context, so a request credential copied here would outlive the
// request and reach code that was never its audience. Subscribers do not
// authenticate with the caller's credential: the gateway-verified principal
// travels in the `x-w17-*` keys.
//
// Multi-value keys collapse to ONE value, chosen the way that key's reader on
// the subscriber side chooses, so the emitter and every subscriber interpret
// the same request identically:
//
//   - the gateway-owned namespaces (`x-w17-*`, `w17-label-*`) keep the FIRST
//     value, as [principal.UserID], [principal.Scope] and [LabelsFromContext]
//     read them (the gateway sets them single-valued anyway);
//   - every other key keeps the LAST value — the metadata convention where a
//     declared binding is appended after any value an earlier layer wrote, and
//     the one the locale readers (`i18n.LocaleFromContext`,
//     `i18n.FormatLocaleFromContext`) follow. Keeping the first value here
//     rendered a subscriber's messages in a different language than the
//     emitter's.
//
// Returns nil when ctx carries no metadata on either side (an emit
// triggered outside an RPC scope). Subscriber dispatch treats nil and
// empty identically.
//
// The generated emit helpers (server tier and gateway tier) call this;
// keeping the body here rather than rendering it into each bundle is what
// keeps the two tiers from drifting apart again.
func MetadataFromContext(ctx context.Context) map[string]string {
	md, ok := contextMD(ctx)
	if !ok {
		return nil
	}
	flat := make(map[string]string, len(md))
	for k, vs := range md {
		if len(vs) == 0 || ReservedMetadataKey(k) {
			continue
		}
		if principal.IsGatewayOwnedKey(k) {
			flat[k] = vs[0]
		} else {
			flat[k] = vs[len(vs)-1]
		}
	}
	return flat
}

// reservedMetadataKeys are transport-internal headers that must never ride an
// event envelope, whatever the originating request happened to carry.
//
// `w17-tx-id` is the one that matters and the reason this list exists. An
// emit is DEFERRED UNTIL COMMIT, so the transaction named in the originating
// request is settled before any subscriber sees the event — while generated
// dispatch replays this map into the subscriber's context wholesale, which
// makes the subscriber look like a participant in a transaction that is over.
// A subscriber never asserts a transaction; one was being handed to it.
//
// Kept as a literal rather than imported from `service/tx/txregistry` on
// purpose, and the import is avoided in BOTH directions: `lib/` does not
// depend on `service/`, and a test-only import the other way would drag NATS
// and its dependencies into the go.sum of every downstream module — `go mod
// tidy` keeps sums sufficient to test dependencies, so plugin modules would
// inherit the broker stack from an assertion. Both ends pin the same literal
// instead; `txregistry.TestHeaderName_IsTheStrippedEnvelopeKey` fails on a
// rename and points here.
//
// `w17-conn-id` travels with the transaction id: it is the routing token that
// pins a call to the storage connection holding the transaction. Replayed into
// a subscriber it would pin the subscriber's own storage calls to a
// connection that belongs to a settled transaction. `distx.ConnIDHeader` is
// pinned the same way (`TestConnIDHeader_IsTheStrippedEnvelopeKey`).
var reservedMetadataKeys = map[string]bool{
	"w17-tx-id":   true,
	"w17-conn-id": true,
}

// credentialHeaders are request headers that carry a credential by
// definition, matched by exact (lower-case) name.
var credentialHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	// The gateway's own slot for a credential lifted out of a URL; it is
	// in the gateway-owned namespace, so it is named here explicitly rather
	// than caught by the segment rule below.
	"x-w17-credential": true,
}

// credentialNameSegments flag a header as credential-bearing when any
// `-`/`_`-separated segment of its name is one of them: `x-api-key`,
// `x-auth-token`, `x-amz-security-token`, `x-session-id`, `x-csrf-token`,
// `x-client-secret`. Whole segments, not substrings, so ordinary names that
// merely contain the letters (`x-author-id`) keep travelling.
var credentialNameSegments = map[string]bool{
	"auth":          true,
	"authorization": true,
	"apikey":        true,
	"cookie":        true,
	"credential":    true,
	"credentials":   true,
	"jwt":           true,
	"passwd":        true,
	"password":      true,
	"secret":        true,
	"session":       true,
	"token":         true,
}

// grpcGatewayHeaderPrefix is the prefix grpc-gateway puts on the permanent
// HTTP headers it forwards (`grpcgateway-cookie`); the name behind it is
// classified like the bare header.
const grpcGatewayHeaderPrefix = "grpcgateway-"

// isCredentialKey reports whether a metadata key carries a request
// credential. The gateway-owned namespaces never do — they hold the verified
// principal, scopes and labels a subscriber needs — so they are exempt from
// the segment rule even when a declared scope or label name happens to
// contain a flagged word; only the exact names above apply to them.
func isCredentialKey(k string) bool {
	k = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(k)), grpcGatewayHeaderPrefix)
	if credentialHeaders[k] {
		return true
	}
	if principal.IsGatewayOwnedKey(k) {
		return false
	}
	segs := strings.FieldsFunc(k, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i, seg := range segs {
		if credentialNameSegments[seg] {
			return true
		}
		if seg == "api" && i+1 < len(segs) && segs[i+1] == "key" {
			return true
		}
	}
	return false
}

// ReservedMetadataKey reports whether a metadata key must be stripped from an
// event envelope: a transport-internal key (`w17-tx-id`) or a request
// credential (`authorization`, `cookie`, `x-api-key`, `*-token`, …).
//
// Generated subscriber dispatch applies the same predicate when it replays
// `Event.Metadata` into the handler's context, so envelopes persisted before
// a key was added here are filtered on delivery too. Exported also so the
// drift test that owns the transaction key's real definition can check this
// list against it.
func ReservedMetadataKey(k string) bool {
	return reservedMetadataKeys[strings.ToLower(k)] || isCredentialKey(k)
}

// LabelsFromContext extracts the originating request's broadcast labels —
// the [LabelKeyPrefix] metadata entries — into a flat map with the prefix
// stripped, for `Event.Labels` (and for the reserved invalidate topic's
// own labels field).
//
// nil means the event is unlabelled. On a surface that partitions its
// events by label that means the event reaches NOBODY (restgw.entitled):
// an event that cannot be scoped to a tenant has no principal it can be
// shown to safely.
func LabelsFromContext(ctx context.Context) map[string]string {
	md, ok := contextMD(ctx)
	if !ok {
		return nil
	}
	var labels map[string]string
	for k, vs := range md {
		if len(vs) == 0 || len(k) <= len(LabelKeyPrefix) || k[:len(LabelKeyPrefix)] != LabelKeyPrefix {
			continue
		}
		if labels == nil {
			labels = map[string]string{}
		}
		labels[k[len(LabelKeyPrefix):]] = vs[0]
	}
	return labels
}
