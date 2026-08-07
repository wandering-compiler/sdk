package principal

import "strings"

// IsGatewayOwnedKey reports whether `key` targets one of the metadata
// namespaces a public gateway OWNS — the ones it writes from the verified auth
// response and every downstream tier trusts (`x-w17-*`, `w17-label-*`).
//
// It is the key-at-a-time reading of the rule [SanitizeGatewayMD] applies to a
// whole context, for the surfaces that assemble their metadata from something
// other than an incoming gRPC context: the MCP transport builds its outgoing
// metadata by forwarding client-influenced HTTP headers / initialize params /
// env named in `W17_MCP_FORWARD_HEADERS`, so there is no incoming side to
// sanitize — there is a list of keys to refuse.
//
// One rule, one list. The gateway parser refuses these prefixes in a declared
// `metadata_binding` / `metadata_propagation`, and the gateway wire path strips
// them; a surface that let one through would be the hole in a fence three other
// surfaces maintain (C8-F8, T2-6 pass #8).
func IsGatewayOwnedKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, p := range gatewayOwnedPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
