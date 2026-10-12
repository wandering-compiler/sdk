package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Over MCP HTTP a client must not put a value in a key the gateway reserves,
// whether the operator listed it in the forwarding list or not, and must not
// hand the auth backend the slots only the gateway fills. The generated
// translator stamps the verified principal over the forwarded map; what it
// does not stamp — a scope the auth response omits, the transaction keys —
// must therefore never be in the map at all.
func TestServeStreamableHTTP_ClientCannotSetReservedMetadata(t *testing.T) {
	forward := []string{
		"X-W17-User", "X-W17-Scope-Tenant_id", "W17-Label-Tenant",
		"W17-Tx-Id", "W17-Conn-Id", "Grpcgateway-Authorization", "Grpc-Metadata-X-W17-User",
		"Session-Id",
	}
	var mu sync.Mutex
	var authHeaders map[string]string
	auth := func(ctx context.Context, c ConnectionInfo) (metadata.MD, error) {
		mu.Lock()
		authHeaders = CollectAuthHeaders(c)
		mu.Unlock()
		return DefaultAuthFunc(forward...)(ctx, c)
	}
	s := NewServer("x", "1", auth)
	var sent metadata.MD
	s.AddTool("t", "d", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return s.CallUnary(ctx, req, &emptypb.Empty{}, func(ctx context.Context, _ proto.Message) (proto.Message, error) {
			mu.Lock()
			sent, _ = metadata.FromOutgoingContext(ctx)
			mu.Unlock()
			return &emptypb.Empty{}, nil
		})
	})

	forged := http.Header{}
	forged.Set("X-W17-User", "forged-principal")
	forged.Set("X-W17-Scope-Tenant_id", "forged-tenant")
	forged.Set("W17-Label-Tenant", "forged-label")
	forged.Set("W17-Tx-Id", "forged-tx")
	forged.Set("W17-Conn-Id", "forged-conn")
	forged.Set("Grpcgateway-Authorization", "forged")
	forged.Set("Grpc-Metadata-X-W17-User", "forged")
	forged.Set("X-W17-Credential", "forged-capability")
	forged.Set("Session-Id", "abc")
	forged.Set("Authorization", "Bearer t")

	base := startHTTP(t, s)
	if resp := callToolHTTP(t, base, "t", forged); !strings.Contains(resp, `"result"`) {
		t.Fatalf("tool call failed: %s", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, k := range []string{
		"x-w17-user", "x-w17-scope-tenant_id", "w17-label-tenant", "w17-tx-id", "w17-conn-id",
		"grpcgateway-authorization", "grpc-metadata-x-w17-user", "x-w17-credential",
	} {
		if v := sent.Get(k); len(v) != 0 {
			t.Errorf("backend metadata %s = %q, want absent", k, v)
		}
		if v, ok := authHeaders[k]; ok {
			t.Errorf("auth backend header %s = %q, want absent: only the gateway fills it", k, v)
		}
	}
	// Decoys: ordinary forwarding and the ordinary credential still work.
	if v := sent.Get("session-id"); len(v) != 1 || v[0] != "abc" {
		t.Errorf("backend session-id = %q, want [abc]", v)
	}
	if authHeaders["authorization"] != "Bearer t" {
		t.Errorf("auth backend authorization = %q, want the caller's bearer", authHeaders["authorization"])
	}
}

// The same rule on every source CollectAuthHeaders merges, not only HTTP.
func TestCollectAuthHeaders_DropsReservedKeysFromEverySource(t *testing.T) {
	stdio := CollectAuthHeaders(ConnectionInfo{
		Env:              map[string]string{"X_W17_CREDENTIAL": "c", "W17_TX_ID": "t", "AUTHORIZATION": "Bearer e"},
		InitializeParams: map[string]any{"x-w17-user": "u", "w17-label-tenant": "l", "session-id": "s"},
	})
	for _, k := range []string{"x-w17-credential", "w17-tx-id", "x-w17-user", "w17-label-tenant"} {
		if v, ok := stdio[k]; ok {
			t.Errorf("%s = %q, want absent", k, v)
		}
	}
	if stdio["authorization"] != "Bearer e" || stdio["session-id"] != "s" {
		t.Errorf("ordinary identity sources lost: %v", stdio)
	}
}
