package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// The HTTP transport as an OAuth 2.1 protected resource — the MCP
// authorization specification: RFC 9728 metadata, RFC 6750
// challenges, and the resource (RFC 8707 audience) told to the auth method.
//
// A client calling the endpoint without a token is answered 401 with a
// WWW-Authenticate naming the metadata document; the document names the
// authorization server; the client signs the user in there and comes back
// with a token issued for this resource. The audience check itself is the
// auth method's: the resource is stamped under [principal.MCPResourceKey]
// next to the surface, and a token issued for another resource is refused
// there like an unknown one.

// ResourceServer configures the protected-resource half (SetResourceServer).
type ResourceServer struct {
	// Resource is the resource identifier: the absolute https URL clients
	// call (`https://mcp.example.com/mcp`). It is the token's audience.
	Resource string
	// AuthorizationServers are the issuers whose tokens this resource takes.
	AuthorizationServers []string
	// ResourceName is the human-readable name in the metadata; optional.
	ResourceName string
	// Scopes are the scopes a token needs; empty = ["mcp"].
	Scopes []string
}

// ResourceServerFromEnv reads a protected resource's deployment settings:
// `<PREFIX>_MCP_PUBLIC_URL` (the resource identifier — the URL clients call)
// and `<PREFIX>_MCP_AUTHORIZATION_SERVER` (the issuer; several separated by
// commas). Both are required: a surface declared a protected resource does
// not serve without them.
func ResourceServerFromEnv(prefix string, getenv func(string) string) (ResourceServer, error) {
	res := strings.TrimSpace(getenv(prefix + "_MCP_PUBLIC_URL"))
	var issuers []string
	for _, v := range strings.Split(getenv(prefix+"_MCP_AUTHORIZATION_SERVER"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			issuers = append(issuers, v)
		}
	}
	if res == "" || len(issuers) == 0 {
		return ResourceServer{}, fmt.Errorf("mcp: this MCP surface is an OAuth protected resource ((w17.mcp_api).oauth): set %s_MCP_PUBLIC_URL (the URL clients call, e.g. https://mcp.example.com/mcp) and %s_MCP_AUTHORIZATION_SERVER (the issuer, e.g. https://auth.example.com)", prefix, prefix)
	}
	return ResourceServer{Resource: res, AuthorizationServers: issuers}, nil
}

// DefaultScope is the one OAuth scope an MCP token carries: what the holder
// may do is the user's mcp-realm roles, not the scope.
const DefaultScope = "mcp"

// ProtectedResourceMetadataPath is RFC 9728's well-known path; the document
// is also served with the resource's path appended (§3.1), which is where a
// client looks first.
const ProtectedResourceMetadataPath = "/.well-known/oauth-protected-resource"

// SetResourceServer turns the HTTP transport into a protected resource. It
// needs the permission resolver (SetPermResolver): the token check IS the
// auth method's answer. Call before serving; an invalid configuration is an
// error so the server refuses to start rather than serve a broken flow.
func (s *Server) SetResourceServer(rs ResourceServer) error {
	u, err := url.Parse(rs.Resource)
	if err != nil || !u.IsAbs() || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("mcp: resource %q is not an absolute URL without query or fragment", rs.Resource)
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("mcp: resource %q must be https (http only on a loopback host)", rs.Resource)
	}
	if len(rs.AuthorizationServers) == 0 {
		return errors.New("mcp: a protected resource names at least one authorization server")
	}
	for _, as := range rs.AuthorizationServers {
		a, err := url.Parse(as)
		if err != nil || !a.IsAbs() || a.Host == "" || (a.Scheme != "https" && !isLoopbackHost(a.Hostname())) {
			return fmt.Errorf("mcp: authorization server %q is not an https URL", as)
		}
	}
	if len(rs.Scopes) == 0 {
		rs.Scopes = []string{DefaultScope}
	}
	s.resource = &rs
	s.resourceURL = u
	return nil
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// metadataURL is where the document for this resource lives: the
// well-known path inserted between the host and the resource's path.
func (s *Server) metadataURL() string {
	u := *s.resourceURL
	u.Path = ProtectedResourceMetadataPath + strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

// mountResourceServer adds the metadata routes to mux.
func (s *Server) mountResourceServer(mux *http.ServeMux) {
	doc, _ := json.Marshal(map[string]any{
		"resource":                 s.resource.Resource,
		"authorization_servers":    s.resource.AuthorizationServers,
		"scopes_supported":         s.resource.Scopes,
		"bearer_methods_supported": []string{"header"},
		"resource_name":            s.resource.ResourceName,
	})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		_, _ = w.Write(doc)
	})
	paths := map[string]bool{
		ProtectedResourceMetadataPath:                true,
		ProtectedResourceMetadataPath + EndpointPath: true,
	}
	if p := strings.TrimSuffix(s.resourceURL.Path, "/"); p != "" {
		paths[ProtectedResourceMetadataPath+p] = true
	}
	for p := range paths {
		mux.Handle(p, h)
	}
}

// requireToken is the gate in front of the MCP endpoint: no bearer, or one
// the auth method refuses, is a 401 challenge; a token that holds no
// permission at all on this surface is a 403 insufficient_scope. Everything
// past it is the per-tool gate, as on a surface without OAuth.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := bearerToken(r.Header.Get("Authorization")); !ok {
			s.challenge(w, http.StatusUnauthorized, "", "")
			return
		}
		conn := ConnectionInfo{
			HTTPHeaders: r.Header,
			ClientAddr:  ratelimit.HTTPClientAddr(r, s.trustedProxies),
			Method:      r.Method + " " + EndpointPath,
			Resource:    s.resource.Resource,
			Gateway:     s.gatewayHeaders,
		}
		perms, err := s.checkToken(r.Context(), conn)
		switch {
		case err == nil && len(perms) == 0:
			s.challenge(w, http.StatusForbidden, "insufficient_scope", "the token holds no permission on this server")
		case err == nil:
			next.ServeHTTP(w, r)
		case isUnauthenticated(err):
			s.challenge(w, http.StatusUnauthorized, "invalid_token", "the token is not valid for this resource")
		default:
			observx.ReportError(r.Context(), fmt.Errorf("mcp: token check: %w", err))
			http.Error(w, "the authorization service is unavailable", http.StatusServiceUnavailable)
		}
	})
}

// isUnauthenticated reports whether the auth method refused the token, as
// opposed to failing: a refusal is the caller's, a failure is ours.
func isUnauthenticated(err error) bool {
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied, codes.NotFound, codes.InvalidArgument:
		return true
	}
	return false
}

// challenge writes the RFC 6750 / RFC 9728 answer.
func (s *Server) challenge(w http.ResponseWriter, code int, errCode, desc string) {
	v := fmt.Sprintf(`Bearer resource_metadata=%q, scope=%q`, s.metadataURL(), strings.Join(s.resource.Scopes, " "))
	if errCode != "" {
		v += fmt.Sprintf(`, error=%q, error_description=%q`, errCode, desc)
	}
	w.Header().Set("WWW-Authenticate", v)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
}

// bearerToken reads `Authorization: Bearer <token>` (scheme case-insensitive).
func bearerToken(h string) (string, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// checkToken asks the auth method about the token on every request, never
// from an earlier answer: a token revoked a moment ago is refused with the
// 401 that sends its client back to authorization, not admitted to an MCP
// session whose every tool call then fails. (The tool calls ask the auth
// method again, for their own method, so an answer kept here would have
// saved nothing but that 401.)
func (s *Server) checkToken(ctx context.Context, conn ConnectionInfo) ([]int32, error) {
	return s.resolveForGate(ctx, conn)
}

// resolveForGate runs the resolver, a panic counted as a failure.
func (s *Server) resolveForGate(ctx context.Context, conn ConnectionInfo) (perms []int32, err error) {
	defer func() {
		if r := recover(); r != nil {
			perms, err = nil, fmt.Errorf("permission resolver panicked: %v", r)
		}
	}()
	return s.permResolver(ctx, conn)
}

// checkResourceServer refuses to serve a protected resource with nothing to
// check tokens with.
func (s *Server) checkResourceServer() error {
	if s.resource != nil && s.permResolver == nil {
		return errors.New("mcp: SetResourceServer needs SetPermResolver (the token check is the auth method's answer)")
	}
	if s.resource != nil {
		log.Printf("mcp: protected resource %s (authorization servers %s)", s.resource.Resource, strings.Join(s.resource.AuthorizationServers, ", "))
	}
	return nil
}
