package principal_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// forgedIncoming is a caller's raw gRPC metadata as it arrives at a public
// rpc gateway: two reserved keys the client must never be able to set, and
// two ordinary headers it may.
func forgedIncoming() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-w17-scope-tenant_id", "victim-tenant",
		"x-w17-user", "forged-envelope",
		"w17-label-tenant_id", "victim-tenant",
		"w17-org", "caller-active-org",
		"traceparent", "00-trace-span-01",
	))
}

// TestWithGatewayMD_SanitizesBothSides is the C8-F2 regression guard.
//
// The gateway's namespace strip protected the copy it forwards to the
// backend and left ctx's INCOMING metadata untouched — still holding the
// client's raw `x-w17-scope-*`, `x-w17-user` and `w17-label-*`. That is
// the side the event-emit path reads: a forged label decides which
// tenant's SSE stream the event lands in, and a forged scope rides
// Event.Metadata into the subscriber's reinjected context and from there
// into the storage auto-WHERE. Sanitizing one side of a context is not
// sanitizing the context.
func TestWithGatewayMD_SanitizesBothSides(t *testing.T) {
	ctx := forgedIncoming()

	md := principal.SanitizeGatewayMD(ctx)
	// The verified principal is stamped onto the sanitized copy, exactly
	// as the generated interceptor does after its auth call.
	md.Set("x-w17-user", "verified-envelope")
	md.Set("x-w17-scope-tenant_id", "real-tenant")
	md.Set(principal.LabelKeyPrefix+"tenant_id", "real-tenant")

	ctx = principal.WithGatewayMD(ctx, md)

	for _, side := range []struct {
		name string
		md   func() (metadata.MD, bool)
	}{
		{"outgoing", func() (metadata.MD, bool) { return metadata.FromOutgoingContext(ctx) }},
		{"incoming", func() (metadata.MD, bool) { return metadata.FromIncomingContext(ctx) }},
	} {
		got, ok := side.md()
		if !ok {
			t.Fatalf("%s metadata missing from the augmented ctx", side.name)
		}
		for key, want := range map[string]string{
			"x-w17-user":            "verified-envelope",
			"x-w17-scope-tenant_id": "real-tenant",
			"w17-label-tenant_id":   "real-tenant",
		} {
			vals := got.Get(key)
			if len(vals) != 1 || vals[0] != want {
				t.Errorf("%s metadata %q = %v, want exactly [%q] — the client's value must be "+
					"gone from BOTH sides, not overwritten on one", side.name, key, vals, want)
			}
		}
		// Non-reserved headers survive untouched: w17-org is a legitimate
		// client header the console validates rather than trusts, and the
		// trace context must keep flowing.
		if v := got.Get("w17-org"); len(v) != 1 || v[0] != "caller-active-org" {
			t.Errorf("%s metadata dropped w17-org = %v; stripping the whole w17- namespace "+
				"breaks the caller's active-organization header", side.name, v)
		}
		if v := got.Get("traceparent"); len(v) != 1 {
			t.Errorf("%s metadata dropped the trace context: %v", side.name, v)
		}
	}
}

// TestWithGatewayMD_UnauthenticatedPathAlsoSanitized — the public
// (exclude_auth) path stamps NOTHING, so it is the one where an
// overwrite-only defence protects nothing at all. The reserved keys must
// simply be absent from both sides.
func TestWithGatewayMD_UnauthenticatedPathAlsoSanitized(t *testing.T) {
	ctx := principal.WithGatewayMD(forgedIncoming(), principal.SanitizeGatewayMD(forgedIncoming()))

	in, _ := metadata.FromIncomingContext(ctx)
	out, _ := metadata.FromOutgoingContext(ctx)
	for _, key := range []string{"x-w17-scope-tenant_id", "x-w17-user", "w17-label-tenant_id"} {
		if v := in.Get(key); len(v) != 0 {
			t.Errorf("unauthenticated path left %q = %v on the INCOMING side", key, v)
		}
		if v := out.Get(key); len(v) != 0 {
			t.Errorf("unauthenticated path left %q = %v on the OUTGOING side", key, v)
		}
	}
}

// TestWithGatewayMD_SidesDoNotAlias — the two sides must not share one map,
// or a later Set on either silently rewrites the other.
func TestWithGatewayMD_SidesDoNotAlias(t *testing.T) {
	ctx := principal.WithGatewayMD(context.Background(), metadata.Pairs("x-w17-user", "verified"))
	in, _ := metadata.FromIncomingContext(ctx)
	in.Set("x-w17-user", "mutated-through-the-incoming-handle")
	out, _ := metadata.FromOutgoingContext(ctx)
	if v := out.Get("x-w17-user"); len(v) != 1 || v[0] != "verified" {
		t.Errorf("mutating the incoming side changed the outgoing side: %v", v)
	}
}
