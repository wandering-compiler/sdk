package principal_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

func TestDataScope(t *testing.T) {
	in := func(kv ...string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs(kv...))
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"no metadata: no audience", context.Background(), ""},
		{"no key: no audience", in("x-w17-user", "u1"), ""},
		{"owner", in(principal.DataScopeMetadataKey, "owner"), principal.DataScopeOwner},
		{"tenant", in(principal.DataScopeMetadataKey, "tenant"), principal.DataScopeTenant},
		{"all", in(principal.DataScopeMetadataKey, "all"), principal.DataScopeAll},
		// A typo must not read as "no audience", which applies every scope
		// too but is indistinguishable from intent.
		{"unknown reads as the narrowest", in(principal.DataScopeMetadataKey, "everything"), principal.DataScopeOwner},
		// Two values are not one range; the request carries none it can name.
		{"two values: none", in(principal.DataScopeMetadataKey, "all", principal.DataScopeMetadataKey, "owner"), ""},
	} {
		if got := principal.DataScope(tc.ctx); got != tc.want {
			t.Errorf("%s: DataScope = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A facade's calls into storage carry the range of the request that made
// them, and a client cannot choose one.
func TestDataScope_RelayedAndGatewayOwned(t *testing.T) {
	ctx := principal.ForwardToOutgoing(metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-w17-user", "u1", principal.DataScopeMetadataKey, principal.DataScopeOwner,
	)))
	out, _ := metadata.FromOutgoingContext(ctx)
	if got := out.Get(principal.DataScopeMetadataKey); len(got) != 1 || got[0] != principal.DataScopeOwner {
		t.Errorf("outgoing data scope = %v, want [owner]", got)
	}

	md := principal.SanitizeGatewayMD(metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		principal.DataScopeMetadataKey, principal.DataScopeAll,
	)))
	if got := md.Get(principal.DataScopeMetadataKey); len(got) != 0 {
		t.Errorf("a client's x-w17-data-scope survived the gateway: %v", got)
	}
}
