package distx_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
)

// The gateways refuse to take the connection-affinity key from a client,
// matching it by the same literal (gatewayOwnedKeys in
// lib/principal/gateway.go); a rename must move both, or a client could pin
// the connection a transaction is routed to.
func TestConnIDHeader_IsGatewayOwned(t *testing.T) {
	if !principal.IsGatewayOwnedKey(distx.ConnIDHeader) || !principal.IsEdgeReservedKey(distx.ConnIDHeader) {
		t.Fatalf("principal does not treat %q as gateway-owned — update gatewayOwnedKeys in lib/principal/gateway.go",
			distx.ConnIDHeader)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(distx.ConnIDHeader, "forged"))
	if v := principal.SanitizeGatewayMD(ctx).Get(distx.ConnIDHeader); len(v) != 0 {
		t.Fatalf("SanitizeGatewayMD kept %s = %v", distx.ConnIDHeader, v)
	}
}
