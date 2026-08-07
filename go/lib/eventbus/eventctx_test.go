package eventbus_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// TestEventContextReaders_GatewayTierReadsOutgoing is the B-F9 / C8-F3
// regression guard.
//
// A gateway-tier emit is called with the context the REST / MCP handler
// just BUILT for its downstream call: `r.Context()` plus OUTGOING metadata
// (restgw.SetUserMetadata + AppendToOutgoingContext for `x-w17-scope-*`
// and `w17-label-*`). There is no incoming gRPC metadata on an http
// request context at all, so a reader that consults only the incoming
// side returns nothing.
//
// The labels reader was fixed for this and the metadata reader beside it
// was not, so every gateway-tier event shipped `Metadata: nil` while its
// `Labels` were populated — the two fields describing different requests.
// Subscriber dispatch reinjects Event.Metadata as the handler call's
// incoming metadata, and that is the only channel carrying
// `x-w17-scope-*` to a scope-guarded storage handler, so a subscriber on
// such an event is denied on every delivery. Both readers must see the
// same side.
func TestEventContextReaders_GatewayTierReadsOutgoing(t *testing.T) {
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"x-w17-user", "envelope-bytes",
		"x-w17-scope-tenant_id", "alpha",
		"w17-label-tenant_id", "alpha",
		"traceparent", "00-trace-span-01",
	)

	md := eventbus.MetadataFromContext(ctx)
	if len(md) == 0 {
		t.Fatalf("gateway-tier emit published EMPTY Event.Metadata; the scope tunnel and the "+
			"trace context are on the OUTGOING side of this ctx: %#v", md)
	}
	if got := md["x-w17-scope-tenant_id"]; got != "alpha" {
		t.Errorf("Event.Metadata[x-w17-scope-tenant_id] = %q, want alpha — a subscriber "+
			"rehydrates this map to reach a scope-guarded handler", got)
	}
	if got := md["traceparent"]; got != "00-trace-span-01" {
		t.Errorf("Event.Metadata[traceparent] = %q, want the originating trace context", got)
	}

	labels := eventbus.LabelsFromContext(ctx)
	if got := labels["tenant_id"]; got != "alpha" {
		t.Errorf("Event.Labels[tenant_id] = %q, want alpha", got)
	}
	if _, leaked := labels["x-w17-scope-tenant_id"]; leaked {
		t.Errorf("labels reader must only surface %q keys, prefix stripped; got %#v",
			eventbus.LabelKeyPrefix, labels)
	}
}

// TestEventContextReaders_ServerTierReadsIncoming — the other tier. A
// storage / business handler RECEIVED its call, so the gateway's stamps
// are incoming. Both readers must land on that same side.
func TestEventContextReaders_ServerTierReadsIncoming(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-w17-scope-tenant_id", "beta",
		"w17-label-tenant_id", "beta",
	))

	if got := eventbus.MetadataFromContext(ctx)["x-w17-scope-tenant_id"]; got != "beta" {
		t.Errorf("server-tier Event.Metadata[x-w17-scope-tenant_id] = %q, want beta", got)
	}
	if got := eventbus.LabelsFromContext(ctx)["tenant_id"]; got != "beta" {
		t.Errorf("server-tier Event.Labels[tenant_id] = %q, want beta", got)
	}
}

// TestEventContextReaders_OneSideOnly pins that the two readers never
// disagree about WHICH side they read: whichever side wins, both fields
// describe the same request. A context with metadata on both sides (the
// rpc gateway tier, where the interceptor installs its sanitized copy on
// both) must not have one field sourced from one side and the other from
// the other.
func TestEventContextReaders_OneSideOnly(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"w17-label-tenant_id", "incoming",
		"origin", "incoming",
	))
	ctx = metadata.AppendToOutgoingContext(ctx,
		"w17-label-tenant_id", "outgoing",
		"origin", "outgoing",
	)

	mdOrigin := eventbus.MetadataFromContext(ctx)["origin"]
	labelOrigin := eventbus.LabelsFromContext(ctx)["tenant_id"]
	if mdOrigin != labelOrigin {
		t.Errorf("Event.Metadata came from the %q side and Event.Labels from the %q side — "+
			"one envelope must describe one request", mdOrigin, labelOrigin)
	}
	if mdOrigin != "incoming" {
		t.Errorf("incoming metadata must win when present; got %q", mdOrigin)
	}
}

// TestEventContextReaders_NoMetadata — an emit outside any RPC scope
// yields nil on both readers (subscriber dispatch treats nil and empty
// identically; an unlabelled event is withheld by an isolating hub).
func TestEventContextReaders_NoMetadata(t *testing.T) {
	if md := eventbus.MetadataFromContext(context.Background()); md != nil {
		t.Errorf("MetadataFromContext(bare ctx) = %#v, want nil", md)
	}
	if l := eventbus.LabelsFromContext(context.Background()); l != nil {
		t.Errorf("LabelsFromContext(bare ctx) = %#v, want nil", l)
	}
}

// An event envelope must not carry the transaction that produced it.
//
// MetadataFromContext flattens the originating request's metadata into
// `Event.Metadata`, and generated dispatch replays that map into the
// subscriber's context wholesale. `w17-tx-id` in that map is therefore
// handed to a subscriber as if the subscriber were part of the emitting
// transaction — which it never is, and cannot be: the emit is deferred
// until commit, so the transaction is settled before any delivery happens.
//
// While AdoptTx fell through to a fresh transaction on an unknown id, the
// replayed id was merely meaningless. Once AdoptTx started refusing unknown
// ids (correctly — silently committing partial state on a fresh transaction
// is worse), the same replay began failing every delivery and every retry of
// every event emitted inside a distributed transaction, silently: emit
// success does not log, and handler errors are Nak'd into a DLQ.
//
// Reported from downstream as "creating an organisation no longer provisions
// its wallet", with no log line anywhere.
func TestMetadataFromContext_DropsTheTransactionHeader(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"w17-tx-id", "tx-42",
		"traceparent", "00-abc-def-01",
		"x-w17-scope-org_id", "org-7",
	))

	md := eventbus.MetadataFromContext(ctx)

	if got, ok := md["w17-tx-id"]; ok {
		t.Errorf("the envelope carries w17-tx-id=%q: dispatch replays this map into the "+
			"subscriber, so the subscriber adopts a transaction that was settled before "+
			"the event was ever delivered", got)
	}
	// Everything an event legitimately carries across must survive: dropping
	// the whole map instead of the reserved keys would take tracing and the
	// scope tunnel with it.
	if md["traceparent"] != "00-abc-def-01" {
		t.Errorf("traceparent = %q, want it carried across", md["traceparent"])
	}
	if md["x-w17-scope-org_id"] != "org-7" {
		t.Errorf("scope metadata = %q, want it carried across", md["x-w17-scope-org_id"])
	}
}
