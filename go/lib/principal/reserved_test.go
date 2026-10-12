package principal_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

func TestIsEdgeReservedKey(t *testing.T) {
	reserved := []string{
		"x-w17-user", "X-W17-User", " x-w17-scope-tenant_id ", "X-W17-Credential",
		"w17-label-tenant", "W17-Label-Tenant",
		"w17-tx-id", "W17-Tx-Id", "w17-conn-id", "W17-CONN-ID",
		"grpcgateway-authorization", "Grpcgateway-Cookie",
		"Grpc-Metadata-X-W17-User", "grpc-metadata-w17-tx-id", "Grpc-Metadata-W17-Label-Tenant",
	}
	for _, k := range reserved {
		if !principal.IsEdgeReservedKey(k) {
			t.Errorf("IsEdgeReservedKey(%q) = false, want true", k)
		}
	}
	// Ordinary keys, including the near misses the rules are exact about:
	// `w17-org` is a legitimate client header, `w17-language` an ordinary
	// rename target, and a tx key only matches by its whole name.
	ordinary := []string{
		"w17-org", "w17-language", "w17-format-locale", "x-request-id", "authorization",
		"x-w17", "w17-tx-id-hint", "w17-txid", "grpc-metadata-x-tenant", "grpcgateway", "",
	}
	for _, k := range ordinary {
		if principal.IsEdgeReservedKey(k) {
			t.Errorf("IsEdgeReservedKey(%q) = true, want false", k)
		}
	}
}

// IsGatewayOwnedKey keeps its narrower meaning: the principal namespaces
// only, case-insensitively.
func TestIsGatewayOwnedKey_StaysNarrow(t *testing.T) {
	for _, k := range []string{"X-W17-User", "w17-label-x"} {
		if !principal.IsGatewayOwnedKey(k) {
			t.Errorf("IsGatewayOwnedKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"w17-tx-id", "w17-conn-id", "grpcgateway-cookie", "w17-org"} {
		if principal.IsGatewayOwnedKey(k) {
			t.Errorf("IsGatewayOwnedKey(%q) = true, want false", k)
		}
	}
}
