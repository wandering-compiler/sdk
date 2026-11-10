package mcp

import (
	"net/http"
	"testing"
)

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

// A browser page on another site — or one whose name was rebound to this
// address (DNS rebinding: the Host header then names the attacker's site
// too) — sends an Origin. The HTTP transport MUST refuse a browser Origin
// it was not told to accept, before a session is opened; a client that is
// not a browser sends none and is served.
func TestServeStreamableHTTP_RefusesAForeignOrigin(t *testing.T) {
	t.Setenv(AllowedOriginsEnv, "")
	s := NewServer("x", "1", nil)
	base := startHTTP(t, s)
	url := base + EndpointPath

	if resp, body := postMCP(t, url, "", initBody); resp.StatusCode != http.StatusOK {
		t.Fatalf("no Origin (a non-browser client): %d %s", resp.StatusCode, body)
	}
	for _, origin := range []string{"https://evil.example", base, "null"} {
		resp, body := postMCP(t, url, "", initBody, http.Header{"Origin": {origin}})
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Mcp-Session-Id") != "" {
			t.Fatalf("Origin %s: %d %s, want 403 and no session", origin, resp.StatusCode, body)
		}
	}
	if n := s.sessions.count(); n != 1 {
		t.Fatalf("%d sessions open, want only the non-browser client's", n)
	}
}

// Listed origins are served, exactly as written (scheme, host and port);
// `*` accepts any; the environment variable lists them when the server
// was not told in code.
func TestServeStreamableHTTP_AllowedOrigins(t *testing.T) {
	s := NewServer("x", "1", nil)
	s.SetAllowedOrigins([]string{"https://inspector.example"})
	url := startHTTP(t, s) + EndpointPath
	if resp, body := postMCP(t, url, "", initBody, http.Header{"Origin": {"https://inspector.example"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a listed origin: %d %s", resp.StatusCode, body)
	}
	if resp, _ := postMCP(t, url, "", initBody, http.Header{"Origin": {"https://inspector.example:8443"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("another port of a listed origin: %d, want 403", resp.StatusCode)
	}

	t.Setenv(AllowedOriginsEnv, " https://a.example , https://b.example ")
	fromEnv := NewServer("x", "1", nil)
	url = startHTTP(t, fromEnv) + EndpointPath
	if resp, body := postMCP(t, url, "", initBody, http.Header{"Origin": {"https://b.example"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("an origin the environment lists: %d %s", resp.StatusCode, body)
	}

	anyS := NewServer("x", "1", nil)
	anyS.SetAllowedOrigins([]string{"*"})
	url = startHTTP(t, anyS) + EndpointPath
	if resp, body := postMCP(t, url, "", initBody, http.Header{"Origin": {"https://anything.example"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("*: %d %s", resp.StatusCode, body)
	}
}
