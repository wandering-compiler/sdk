package eventbus_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/eventbus"
)

// A DPoP proof (the `dpop` metadata key a DPoP client sends next to
// `authorization: DPoP <token>`) is a credential: a signed JWS bound to the
// request. It MUST NOT ride Event.Metadata into the broker stream, the
// dead-letter stream and every subscriber's context.
func TestMetadataFromContext_DropsTheDPoPProof(t *testing.T) {
	for _, k := range []string{"dpop", "DPoP", "grpcgateway-dpop"} {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			k, "eyJ0eXAiOiJkcG9wK2p3dCJ9.proof.sig",
			"x-w17-user", "u1",
		))
		md := eventbus.MetadataFromContext(ctx)
		for got := range md {
			if got == "dpop" || got == "grpcgateway-dpop" {
				t.Errorf("%s: the DPoP proof rides the envelope: %v", k, md)
			}
		}
		if md["x-w17-user"] != "u1" {
			t.Errorf("%s: the principal no longer travels: %v", k, md)
		}
		if !eventbus.ReservedMetadataKey(k) {
			t.Errorf("ReservedMetadataKey(%q) = false; generated dispatch would replay it", k)
		}
	}
}

// An empty label value is no label: principal.Label reads "" as absent, so
// an event labelled {org: ""} would otherwise demand a value no principal
// can possess — or, compared by map lookup, be granted to every principal
// lacking the label.
func TestLabelsFromContext_DropsEmptyValues(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"w17-label-org", "",
		"w17-label-account_kind", "person",
	))
	got := eventbus.LabelsFromContext(ctx)
	if _, ok := got["org"]; ok || got["account_kind"] != "person" || len(got) != 1 {
		t.Fatalf("labels = %#v, want only account_kind=person", got)
	}
	ctx = metadata.AppendToOutgoingContext(context.Background(), "w17-label-org", "")
	if got := eventbus.LabelsFromContext(ctx); len(got) != 0 {
		t.Fatalf("an all-empty label set = %#v, want none", got)
	}
}
