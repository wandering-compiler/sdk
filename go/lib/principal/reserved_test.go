package principal_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

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

// IsGatewayOwnedKey covers the principal namespaces by prefix and the two
// transaction keys by whole name, case-insensitively; the grpc-gateway
// spellings are the edge predicate's addition only.
func TestIsGatewayOwnedKey(t *testing.T) {
	for _, k := range []string{"X-W17-User", "w17-label-x", "w17-tx-id", "W17-Tx-Id", "w17-conn-id", "W17-CONN-ID"} {
		if !principal.IsGatewayOwnedKey(k) {
			t.Errorf("IsGatewayOwnedKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"w17-tx-id-hint", "w17-txid", "w17-conn", "grpcgateway-cookie", "w17-org"} {
		if principal.IsGatewayOwnedKey(k) {
			t.Errorf("IsGatewayOwnedKey(%q) = true, want false", k)
		}
	}
}

// The rpc transport hands the backend a sanitized copy of the client's
// metadata: the transaction keys must be gone from it like the principal
// namespaces, whatever their case, while a key that only starts with the
// same letters stays.
func TestSanitizeGatewayMD_StripsTransactionKeys(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{
		"w17-tx-id":      {"forged-tx"},
		"W17-Conn-Id":    {"forged-conn"},
		"x-w17-user":     {"forged"},
		"w17-tx-id-hint": {"kept"},
		"w17-org":        {"org-1"},
	})
	md := principal.SanitizeGatewayMD(ctx)
	for _, k := range []string{"w17-tx-id", "W17-Conn-Id", "w17-conn-id", "x-w17-user"} {
		if v, ok := md[k]; ok {
			t.Errorf("sanitized md[%s] = %v, want absent", k, v)
		}
	}
	for _, k := range []string{"w17-tx-id-hint", "w17-org"} {
		if len(md[k]) != 1 {
			t.Errorf("sanitized md[%s] = %v, want kept (decoy)", k, md[k])
		}
	}
}
