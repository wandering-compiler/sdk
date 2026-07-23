package mcp

import (
	"context"
	"testing"
)

// Q63-mcp-1: SetAuthEnvPrefix snapshots only `<prefix>_*` env vars (prefix
// stripped, SCREAMING_SNAKE-keyed) onto every ConnectionInfo the server
// builds — so stdio auth sources the token from env while the unscoped
// environment is never forwarded. Asserted through connInfo +
// CollectAuthHeaders, the exact path resolvePermsSafe / CallUnary use.
func TestSetAuthEnvPrefix_ScopesAndStrips(t *testing.T) {
	t.Setenv("MYAPP_AUTHORIZATION", "Bearer xyz")
	t.Setenv("MYAPP_X_API_KEY", "secret")
	t.Setenv("UNRELATED_SECRET", "should-not-leak")

	s := NewServer("display", "0.1.0", nil)
	s.SetAuthEnvPrefix("MYAPP")

	headers := CollectAuthHeaders(s.connInfo(context.Background()))

	if headers["authorization"] != "Bearer xyz" {
		t.Errorf("scoped env var must reach auth headers, got %v", headers)
	}
	if headers["x-api-key"] != "secret" {
		t.Errorf("multi-word scoped env var must map to x-api-key, got %v", headers)
	}
	if _, leaked := headers["unrelated-secret"]; leaked {
		t.Errorf("unscoped env var must NOT be forwarded, got %v", headers)
	}
	if _, leaked := headers["secret"]; leaked {
		t.Errorf("unscoped env var (prefix-stripped form) must NOT leak, got %v", headers)
	}
}

// An empty prefix leaves env forwarding off (no snapshot) — a server
// without auth doesn't accidentally forward env vars.
func TestSetAuthEnvPrefix_EmptyIsNoop(t *testing.T) {
	t.Setenv("MYAPP_AUTHORIZATION", "Bearer xyz")
	s := NewServer("display", "0.1.0", nil)
	s.SetAuthEnvPrefix("")
	if s.authEnv != nil {
		t.Fatalf("empty prefix must not snapshot env, got %v", s.authEnv)
	}
	if got := CollectAuthHeaders(s.connInfo(context.Background())); len(got) != 0 {
		t.Fatalf("no env forwarding without a prefix, got %v", got)
	}
}
