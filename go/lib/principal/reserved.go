package principal

import "strings"

// IsGatewayOwnedKey reports whether `key` targets metadata a public gateway
// OWNS: the namespaces it writes from the verified auth response and every
// downstream tier trusts (`x-w17-*`, `w17-label-*`), and the transaction
// routing keys only the bundle's own transaction coordinator writes
// (`w17-tx-id`, `w17-conn-id`, matched on the whole name). Case-insensitive.
//
// It is the key-at-a-time reading of the rule [SanitizeGatewayMD] applies to a
// whole context, for the surfaces that assemble their metadata from something
// other than an incoming gRPC context: the MCP transport builds its outgoing
// metadata by forwarding client-influenced HTTP headers / initialize params /
// env named in `W17_MCP_FORWARD_HEADERS`, so there is no incoming side to
// sanitize — there is a list of keys to refuse.
//
// One rule, one list. The gateway parser refuses these keys in a declared
// `metadata_binding` / `metadata_propagation`, and the gateway wire path strips
// them; a surface that let one through would be the hole in a fence three other
// surfaces maintain.
func IsGatewayOwnedKey(key string) bool {
	k := strings.TrimSpace(key)
	for _, p := range gatewayOwnedPrefixes {
		if hasPrefixFold(k, p) {
			return true
		}
	}
	for _, name := range gatewayOwnedKeys {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// IsEdgeReservedKey reports whether `key` — a gRPC metadata key or the HTTP
// header that would become one — must never be filled from client input at a
// public edge. It is the predicate every edge applies before client headers
// reach a backend: the REST gateway's inbound header scrub and its
// propagation / rename / default-stamp middlewares, and the MCP transport's
// forwarding and auth-header collection.
//
// It is [IsGatewayOwnedKey] widened by the spellings a client could reach a
// trusted key by through a grpc-gateway in front of a backend:
//
//   - `grpcgateway-*`, the namespace grpc-gateway writes the permanent HTTP
//     headers it forwards under (`grpcgateway-authorization`). A backend that
//     reads one believes a gateway put it there.
//   - the `Grpc-Metadata-<key>` HTTP form of a gateway-owned key, which
//     grpc-gateway turns into the bare `<key>`.
func IsEdgeReservedKey(key string) bool {
	k := strings.TrimSpace(key)
	if hasPrefixFold(k, grpcGatewayPrefix) {
		return true
	}
	if hasPrefixFold(k, grpcMetadataHeaderPrefix) {
		k = k[len(grpcMetadataHeaderPrefix):]
	}
	return IsGatewayOwnedKey(k)
}

const (
	// grpcGatewayPrefix is the metadata namespace grpc-gateway forwards
	// permanent HTTP headers under.
	grpcGatewayPrefix = "grpcgateway-"
	// grpcMetadataHeaderPrefix is the HTTP header prefix grpc-gateway
	// strips to produce a bare metadata key.
	grpcMetadataHeaderPrefix = "grpc-metadata-"
)

// hasPrefixFold is strings.HasPrefix ignoring ASCII case, without the
// allocation of lower-casing the key first — the REST scrub runs it over
// every header of every request.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}
