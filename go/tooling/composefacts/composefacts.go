// Package composefacts names the facts about a domain's proto files that decide
// whether a composed binary can be built for that domain. The facts are which
// surfaces a file declares and whether it is an audience registry.
//
// Two places read them. The compiler derives a domain's gateway and admin
// targets by looking for these markers. A client that sends a `compose_binary`
// edit can send each file reduced to the markers it contains (Extract), not its
// full text. A project's domain protos can exceed one gRPC message, while the
// extract is a few bytes per file. Both sides use the same list, so neither one
// can look for a marker the other does not carry.
package composefacts

import "strings"

// The markers, as the compiler matches them: a substring test on the file's
// text.
const (
	RestAPI    = "option (w17.rest_api)"
	RPCAPI     = "option (w17.rpc_api)"
	MCPAPI     = "option (w17.mcp_api)"
	AdminAPI   = "option (w17.admin_api)"
	PrivateAPI = "option (w17.private_api)"
	// Audience marks a possible audience registry (`rest_<audience>.proto`, …).
	// The file name supplies the audience itself.
	Audience = "audience"
)

// Markers is every marker the composition check reads.
var Markers = []string{RestAPI, RPCAPI, MCPAPI, AdminAPI, PrivateAPI, Audience}

// Extract returns body reduced to the markers it contains, one per line, in
// Markers order. A substring test for any marker gives the same answer on the
// extract as on the full text. Nil when body contains none.
func Extract(body []byte) []byte {
	text := string(body)
	var b strings.Builder
	for _, m := range Markers {
		if strings.Contains(text, m) {
			b.WriteString(m)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return []byte(b.String())
}
