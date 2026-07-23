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
