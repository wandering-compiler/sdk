package mcp

import (
	"net/http"
	"os"
	"strings"
)

// AllowedOriginsEnv lists, comma-separated, the browser origins the HTTP
// transport accepts when [Server.SetAllowedOrigins] was not called; `*`
// accepts any. Unset accepts none.
const AllowedOriginsEnv = "W17_MCP_ALLOWED_ORIGINS"

// SetAllowedOrigins names the browser origins (`https://host[:port]`, as a
// browser sends them) the HTTP transport accepts; `*` accepts any. A
// request that carries no Origin — every client that is not a browser — is
// unaffected.
//
// The default accepts no browser origin, not even the endpoint's own: a
// page whose name an attacker rebound to this address (DNS rebinding) is
// "same-origin" by its Host header, so the only origin that can be trusted
// is one the deployment named. The MCP transport specification requires
// servers to validate Origin for this reason.
func (s *Server) SetAllowedOrigins(origins []string) {
	s.allowedOrigins = normalizeOrigins(origins)
	s.originsSet = true
}

func normalizeOrigins(origins []string) map[string]bool {
	out := map[string]bool{}
	for _, o := range origins {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			out[strings.ToLower(o)] = true
		}
	}
	return out
}

// resolvedAllowedOrigins is SetAllowedOrigins' list, else the environment's.
func (s *Server) resolvedAllowedOrigins() map[string]bool {
	if s.originsSet {
		return s.allowedOrigins
	}
	return normalizeOrigins(strings.Split(os.Getenv(AllowedOriginsEnv), ","))
}

// originGuard refuses, with 403 and before anything else on the endpoint
// runs (no session, no token check, no body read), a request whose Origin
// is not allowed.
func originGuard(allowed map[string]bool, next http.Handler) http.Handler {
	anyOrigin := allowed["*"]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !anyOrigin && !allowed[strings.ToLower(strings.TrimRight(origin, "/"))] {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
