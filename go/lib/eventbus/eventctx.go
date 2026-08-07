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
// flat string→string map for `Event.Metadata`. Multi-value headers collapse
// to their first value — every header the runtime stack actually cares
// about (W3C traceparent / tracestate, baggage, scope tunnels, custom
// routing keys) is single-valued in practice; widening to repeated values
// is a proto schema change away if a real case shows up.
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
		if ReservedMetadataKey(k) {
			continue
		}
		if len(vs) > 0 {
			flat[k] = vs[0]
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
var reservedMetadataKeys = map[string]bool{
	"w17-tx-id": true,
}

// ReservedMetadataKey reports whether a metadata key is transport-internal
// and must be stripped from an event envelope. Exported so the drift test
// that owns the key's real definition can check this list against it.
func ReservedMetadataKey(k string) bool {
	return reservedMetadataKeys[strings.ToLower(k)]
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
