package mcp_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/mcp"
)

// G3i3-GW-MCP: DefaultAuthFunc forwards a key from the HTTP
// header → gRPC metadata in lowercase form.
func TestDefaultAuthFunc_HTTPHeader(t *testing.T) {
	fn := mcp.DefaultAuthFunc("Session-Id")
	conn := mcp.ConnectionInfo{HTTPHeaders: http.Header{"Session-Id": []string{"abc-123"}}}
	md, err := fn(context.Background(), conn)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	got := md.Get("session-id")
	if len(got) != 1 || got[0] != "abc-123" {
		t.Errorf("md[session-id] = %v, want [abc-123]", got)
	}
}

// G3i3-GW-MCP: env-var fallback for stdio transport. Key
// "session_id" → env "SESSION_ID".
func TestDefaultAuthFunc_EnvFallback(t *testing.T) {
	t.Setenv("SESSION_ID", "from-env")
	fn := mcp.DefaultAuthFunc("session_id")
	md, err := fn(context.Background(), mcp.ConnectionInfo{})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	got := md.Get("session_id")
	if len(got) != 1 || got[0] != "from-env" {
		t.Errorf("md[session_id] = %v, want [from-env]", got)
	}
}

// G3i3-GW-MCP: HTTPHeaders wins over env (stronger signal —
// caller deliberately sent the header). Use the HTTP-form
// key ("Session-Id") which canonicalises to itself; the env-
// form ("SESSION_ID") is the ENV fallback.
func TestDefaultAuthFunc_HTTPWinsOverEnv(t *testing.T) {
	t.Setenv("SESSION_ID", "from-env")
	fn := mcp.DefaultAuthFunc("Session-Id")
	conn := mcp.ConnectionInfo{HTTPHeaders: http.Header{"Session-Id": []string{"from-header"}}}
	md, err := fn(context.Background(), conn)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	got := md.Get("session-id")
	if len(got) != 1 || got[0] != "from-header" {
		t.Errorf("md[session-id] = %v, want [from-header]", got)
	}
}

// G3i3-GW-MCP: missing key → empty metadata (no error).
func TestDefaultAuthFunc_MissingKey(t *testing.T) {
	fn := mcp.DefaultAuthFunc("missing")
	md, err := fn(context.Background(), mcp.ConnectionInfo{})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if len(md) != 0 {
		t.Errorf("md should be empty; got %v", md)
	}
}

// G3i3-GW-MCP: HTTPHeadersFromContext returns nil for stdio
// path (no header value stashed).
func TestHTTPHeadersFromContext_Empty(t *testing.T) {
	if mcp.HTTPHeadersFromContext(context.Background()) != nil {
		t.Error("expected nil headers for empty context")
	}
}

// DefaultAuthFunc resolves a key from the MCP initialize handshake params
// (the second lookup source, after HTTP headers) using the lowercase form.
func TestDefaultAuthFunc_InitializeParams(t *testing.T) {
	fn := mcp.DefaultAuthFunc("Session-Id")
	conn := mcp.ConnectionInfo{InitializeParams: map[string]any{"session-id": "from-init"}}
	md, err := fn(context.Background(), conn)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got := md.Get("session-id"); len(got) != 1 || got[0] != "from-init" {
		t.Errorf("md[session-id] = %v, want [from-init]", got)
	}
}

// DefaultAuthFunc resolves from an explicit Env map (the test-injection
// source), converting the metadata-style key to SCREAMING_SNAKE — exercising
// envKey's hyphen→underscore branch via a hyphenated key.
func TestDefaultAuthFunc_ExplicitEnvHyphenKey(t *testing.T) {
	fn := mcp.DefaultAuthFunc("session-id")
	conn := mcp.ConnectionInfo{Env: map[string]string{"SESSION_ID": "from-map"}}
	md, err := fn(context.Background(), conn)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got := md.Get("session-id"); len(got) != 1 || got[0] != "from-map" {
		t.Errorf("md[session-id] = %v, want [from-map]", got)
	}
}

// C8-F8 (T2-6 pass #8) — a forwarded header may not target the
// gateway-owned metadata namespaces.
//
// The MCP auth translator builds its outgoing metadata by forwarding the
// client-influenced headers named in W17_MCP_FORWARD_HEADERS, and the generated
// wrapper then STAMPS the verified principal over that same map. Overwriting is
// not enough, which is the rule the rpc transport's sanitize documents: the
// stamp writes `x-w17-scope-<name>` once per scope the auth response actually
// RETURNS, so any scope it omits keeps the client's value — and omission is the
// deliberate fail-closed branch of an ambiguous scope resolver. A caller
// belonging to two orgs, whose resolver therefore stamps nothing, could supply
// `x-w17-scope-org_id` itself and have the storage auto-WHERE run against the
// org they picked. `w17-label-*` is the same hole aimed at an event's audience.
//
// The same key routed through a declared `metadata_propagation` is refused at
// parse time; this is the one channel that reached none of those call sites.
func TestDefaultAuthFunc_DropsGatewayOwnedNamespaces(t *testing.T) {
	fn := mcp.DefaultAuthFunc("x-w17-scope-org_id", "X-W17-User", "w17-label-tenant_id", "w17-org", "Session-Id")
	conn := mcp.ConnectionInfo{HTTPHeaders: http.Header{
		"X-W17-Scope-Org_id":  []string{"forged-org"},
		"X-W17-User":          []string{"forged-envelope"},
		"W17-Label-Tenant_id": []string{"forged-tenant"},
		// The legitimate client header the exact-prefix rule protects: the
		// caller's ACTIVE organization, which the auth backend validates
		// against membership before trusting. A wholesale `w17-` ban would
		// break it.
		"W17-Org":    []string{"org-42"},
		"Session-Id": []string{"abc-123"},
	}}
	md, err := fn(context.Background(), conn)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	for _, forged := range []string{"x-w17-scope-org_id", "x-w17-user", "w17-label-tenant_id"} {
		if got := md.Get(forged); len(got) != 0 {
			t.Errorf("md[%s] = %v, want none — a client-routed value in a gateway-owned namespace survives any stamp that omits that key", forged, got)
		}
	}
	if got := md.Get("w17-org"); len(got) != 1 || got[0] != "org-42" {
		t.Errorf("md[w17-org] = %v, want [org-42] — the prefix is exact, not `w17-`", got)
	}
	if got := md.Get("session-id"); len(got) != 1 || got[0] != "abc-123" {
		t.Errorf("md[session-id] = %v, want [abc-123] — ordinary forwarding must be untouched", got)
	}
}
