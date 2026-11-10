package privatesurface_test

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	"github.com/wandering-compiler/sdk/go/lib/privatesurface"
)

// ForwardedKeys is a security allowlist: adding a key lets every admitted
// domain state that fact on a person's behalf. The exact set is pinned, and
// the device id and the data scope — stamped by a gateway from its own
// verification, never stated by a caller — must never cross.
func TestForwardedKeys_IsExactlyTheStatedFacts(t *testing.T) {
	want := []string{
		"x-w17-audience",
		"x-w17-client-addr",
		"x-w17-dpop-jkt",
		"x-w17-host",
		"x-w17-mcp-resource",
		"x-w17-membership-kinds",
		"x-w17-method",
		"x-w17-org-from",
		"x-w17-product",
		"x-w17-surface",
	}
	got := slices.Sorted(slices.Values(privatesurface.ForwardedKeys))
	if !slices.Equal(got, want) {
		t.Fatalf("ForwardedKeys = %v\nwant %v\n(a change here widens what an admitted caller may state; review it, then update this list)", got, want)
	}

	alpha := privatesurface.Identity{Domain: "alpha", Bundle: "alpha-admin"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		principal.DeviceMetadataKey, "device-forged",
		principal.DataScopeMetadataKey, "all",
		"x-w17-host", "app.example",
	))
	md, err := privatesurface.Forwarded(ctx, alpha)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{principal.DeviceMetadataKey, principal.DataScopeMetadataKey} {
		if len(md.Get(k)) > 0 {
			t.Errorf("Forwarded passed on %s; a caller cannot state it", k)
		}
	}
	if len(md.Get("x-w17-host")) != 1 {
		t.Errorf("Forwarded dropped x-w17-host: %v", md)
	}
}
