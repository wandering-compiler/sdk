package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

func protectedServer(t *testing.T, resolve PermResolver) (*Server, http.Handler) {
	t.Helper()
	s := NewServer("fin", "v1", nil)
	s.SetPermResolver(resolve)
	if err := s.SetResourceServer(ResourceServer{
		Resource:             "https://mcp.example.com/mcp",
		AuthorizationServers: []string{"https://auth.example.com"},
		ResourceName:         "Fin",
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.mountResourceServer(mux)
	mux.Handle(EndpointPath, s.requireToken(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))
	return s, mux
}

func call(h http.Handler, method, path, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The metadata is served at the bare well-known path and with the
// resource's path appended (RFC 9728 §3.1), readable cross-origin.
func TestResourceServer_Metadata(t *testing.T) {
	_, h := protectedServer(t, func(context.Context, ConnectionInfo) ([]int32, error) { return nil, nil })
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		w := call(h, http.MethodGet, p, "")
		if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: %d %v", p, w.Code, w.Header())
		}
		var doc map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["resource"] != "https://mcp.example.com/mcp" ||
			doc["authorization_servers"].([]any)[0] != "https://auth.example.com" ||
			doc["scopes_supported"].([]any)[0] != "mcp" ||
			doc["bearer_methods_supported"].([]any)[0] != "header" {
			t.Errorf("%s: %v", p, doc)
		}
	}
}

// No bearer: 401 naming the metadata, no error code (RFC 6750 §3.1: a
// request without credentials carries none). The auth method is not asked.
func TestResourceServer_NoTokenIsAChallenge(t *testing.T) {
	var asked atomic.Int32
	_, h := protectedServer(t, func(context.Context, ConnectionInfo) ([]int32, error) { asked.Add(1); return []int32{1}, nil })
	for _, auth := range []string{"", "Basic abc", "Bearer ", "DPoP abc"} {
		w := call(h, http.MethodPost, "/mcp", auth)
		got := w.Header().Get("WWW-Authenticate")
		if w.Code != http.StatusUnauthorized ||
			got != `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp", scope="mcp"` {
			t.Errorf("auth %q: %d %q", auth, w.Code, got)
		}
	}
	if asked.Load() != 0 {
		t.Errorf("the auth method was asked %d times for no token", asked.Load())
	}
}

// A token the auth method refuses is invalid_token; one that holds nothing
// here is insufficient_scope; a working one passes; a failing auth service
// is a 503, never a challenge that sends the user to sign in again.
func TestResourceServer_TokenOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		perms   []int32
		err     error
		code    int
		errCode string
	}{
		{"refused", nil, status.Error(codes.Unauthenticated, "no"), http.StatusUnauthorized, `error="invalid_token"`},
		{"no permission", nil, nil, http.StatusForbidden, `error="insufficient_scope"`},
		{"ok", []int32{7}, nil, http.StatusTeapot, ""},
		{"auth down", nil, status.Error(codes.Unavailable, "down"), http.StatusServiceUnavailable, ""},
		{"plain error", nil, errors.New("boom"), http.StatusServiceUnavailable, ""},
	}
	for _, c := range cases {
		_, h := protectedServer(t, func(context.Context, ConnectionInfo) ([]int32, error) { return c.perms, c.err })
		w := call(h, http.MethodPost, "/mcp", "Bearer tok-"+c.name)
		if w.Code != c.code || !strings.Contains(w.Header().Get("WWW-Authenticate"), c.errCode) {
			t.Errorf("%s: %d %q", c.name, w.Code, w.Header().Get("WWW-Authenticate"))
		}
		if c.errCode == "" && w.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s: unexpected challenge %q", c.name, w.Header().Get("WWW-Authenticate"))
		}
	}
}

// The auth method is told the resource, under the gateway-owned key, so it
// can refuse a token issued for another; a client cannot send that key.
func TestResourceServer_ResourceReachesTheAuthMethod(t *testing.T) {
	var seen map[string]string
	_, h := protectedServer(t, func(_ context.Context, conn ConnectionInfo) ([]int32, error) {
		seen = CollectAuthHeaders(conn)
		return []int32{1}, nil
	})
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer t")
	r.Header.Set("X-W17-Mcp-Resource", "https://evil.example/mcp")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen[principal.MCPResourceKey] != "https://mcp.example.com/mcp" || seen[principal.SurfaceKey] != principal.SurfaceMCP {
		t.Errorf("auth headers = %v", seen)
	}
}

// One token's answer is reused for a short while: a session's requests do
// not each cost an auth call. A failure is not kept.
func TestResourceServer_CheckIsReused(t *testing.T) {
	var asked atomic.Int32
	fail := true
	_, h := protectedServer(t, func(context.Context, ConnectionInfo) ([]int32, error) {
		asked.Add(1)
		if fail {
			return nil, status.Error(codes.Unavailable, "down")
		}
		return []int32{1}, nil
	})
	call(h, http.MethodPost, "/mcp", "Bearer t")
	fail = false
	for range 3 {
		if w := call(h, http.MethodPost, "/mcp", "Bearer t"); w.Code != http.StatusTeapot {
			t.Fatalf("code %d", w.Code)
		}
	}
	if asked.Load() != 2 {
		t.Errorf("auth asked %d times, want 2 (the failure, then one answer reused)", asked.Load())
	}
}

func TestSetResourceServer_RefusesABrokenConfiguration(t *testing.T) {
	bad := []ResourceServer{
		{Resource: "", AuthorizationServers: []string{"https://a.example"}},
		{Resource: "/mcp", AuthorizationServers: []string{"https://a.example"}},
		{Resource: "http://mcp.example.com/mcp", AuthorizationServers: []string{"https://a.example"}},
		{Resource: "https://mcp.example.com/mcp?x=1", AuthorizationServers: []string{"https://a.example"}},
		{Resource: "https://mcp.example.com/mcp"},
		{Resource: "https://mcp.example.com/mcp", AuthorizationServers: []string{"http://a.example"}},
	}
	for _, rs := range bad {
		if err := NewServer("x", "1", nil).SetResourceServer(rs); err == nil {
			t.Errorf("accepted %+v", rs)
		}
	}
	if err := NewServer("x", "1", nil).SetResourceServer(ResourceServer{
		Resource: "http://localhost:8081/mcp", AuthorizationServers: []string{"http://127.0.0.1:8080"},
	}); err != nil {
		t.Errorf("loopback dev configuration refused: %v", err)
	}
	s := NewServer("x", "1", nil)
	_ = s.SetResourceServer(ResourceServer{Resource: "https://m.example/mcp", AuthorizationServers: []string{"https://a.example"}})
	if err := s.checkResourceServer(); err == nil {
		t.Error("a protected resource without a permission resolver must not serve")
	}
}

// A failed tool call is a tool result the model reads, not a JSON-RPC error.
func TestToolErrorResult(t *testing.T) {
	r := toolErrorResult(status.Error(codes.NotFound, "Not found."))
	if !r.IsError || len(r.Content) != 1 {
		t.Fatalf("%+v", r)
	}
	b, _ := json.Marshal(r.Content[0])
	if !strings.Contains(string(b), "NotFound: Not found.") {
		t.Errorf("%s", b)
	}
	if b, _ := json.Marshal(toolErrorResult(errors.New("secret detail")).Content[0]); strings.Contains(string(b), "secret") {
		t.Errorf("a non-status error leaked: %s", b)
	}
}

func TestResourceServerFromEnv(t *testing.T) {
	env := map[string]string{"FIN_MCP_PUBLIC_URL": "https://mcp.example.com/mcp", "FIN_MCP_AUTHORIZATION_SERVER": " https://a.example , https://b.example"}
	rs, err := ResourceServerFromEnv("FIN", func(k string) string { return env[k] })
	if err != nil || rs.Resource != "https://mcp.example.com/mcp" || len(rs.AuthorizationServers) != 2 || rs.AuthorizationServers[1] != "https://b.example" {
		t.Fatalf("%+v %v", rs, err)
	}
	if _, err := ResourceServerFromEnv("FIN", func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "FIN_MCP_PUBLIC_URL") {
		t.Errorf("missing settings: %v", err)
	}
}
