package mcp_test

import (
	"net/http"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/mcp"
)

// CollectAuthHeaders merges every identity source a
// ConnectionInfo carries, so the generated MCP authFn works on stdio
// (Env / InitializeParams) as well as HTTP (HTTPHeaders) — not just the
// HTTP-header-only path that breaks stdio auth.
func TestCollectAuthHeaders_MergesAllSources(t *testing.T) {
	// stdio: env + initialize params.
	conn := mcp.ConnectionInfo{
		Env:              map[string]string{"AUTHORIZATION": "Bearer env", "X_API_KEY": "k1"},
		InitializeParams: map[string]any{"session-id": "s1", "ignored-int": 7},
	}
	got := mcp.CollectAuthHeaders(conn)

	want := map[string]string{
		"authorization": "Bearer env", // env, reversed SCREAMING->header
		"x-api-key":     "k1",         // env underscore -> dash
		"session-id":    "s1",         // initialize param (string only)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %q = %q, want %q (full: %v)", k, got[k], v, got)
		}
	}
	if _, leaked := got["ignored-int"]; leaked {
		t.Errorf("non-string InitializeParams value must be skipped, got %v", got)
	}

	// HTTP: headers + initialize params; env is not an identity source.
	conn.HTTPHeaders = http.Header{"Trace-Id": []string{"t1"}}
	got = mcp.CollectAuthHeaders(conn)
	if got["trace-id"] != "t1" || got["session-id"] != "s1" {
		t.Errorf("http conn must carry headers + initialize params, got %v", got)
	}
	if _, ok := got["authorization"]; ok {
		t.Errorf("http conn must not carry env-derived headers, got %v", got)
	}
}

// HTTP headers win over InitializeParams win over Env for the same key.
func TestCollectAuthHeaders_Precedence(t *testing.T) {
	conn := mcp.ConnectionInfo{
		Env:              map[string]string{"AUTHORIZATION": "from-env"},
		InitializeParams: map[string]any{"authorization": "from-init"},
		HTTPHeaders:      http.Header{"Authorization": []string{"from-http"}},
	}
	if got := mcp.CollectAuthHeaders(conn)["authorization"]; got != "from-http" {
		t.Fatalf("HTTP header must win, got %q", got)
	}

	conn.HTTPHeaders = nil // stdio: InitializeParams beats Env
	if got := mcp.CollectAuthHeaders(conn)["authorization"]; got != "from-init" {
		t.Fatalf("InitializeParams must beat Env, got %q", got)
	}

	conn.InitializeParams = nil // pure stdio env
	if got := mcp.CollectAuthHeaders(conn)["authorization"]; got != "from-env" {
		t.Fatalf("Env must be used when it's the only source, got %q", got)
	}
}

// Empty ConnectionInfo yields only the surface the gateway stamps — no
// panic, never reads os.Environ on its own.
func TestCollectAuthHeaders_EmptyConn(t *testing.T) {
	if got := mcp.CollectAuthHeaders(mcp.ConnectionInfo{}); len(got) != 1 || got["x-w17-surface"] != "mcp" {
		t.Fatalf("empty conn must yield the surface alone, got %v", got)
	}
}
