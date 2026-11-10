package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// The surface, the caller's address and the method are the gateway's: a
// client sending them as headers (or, on stdio, as env) changes nothing.
func TestCollectAuthHeaders_StampsTheRequestContext(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer t")
	h.Set("X-W17-Client-Addr", "1.1.1.1")
	h.Set("X-W17-Surface", "rest")
	h.Set("X-W17-Method", "/evil/Method")
	got := CollectAuthHeaders(ConnectionInfo{HTTPHeaders: h, ClientAddr: "203.0.113.7", Method: "/pkg.Svc/M"})
	for k, want := range map[string]string{
		"authorization":         "Bearer t",
		principal.SurfaceKey:    principal.SurfaceMCP,
		principal.ClientAddrKey: "203.0.113.7",
		principal.MethodKey:     "/pkg.Svc/M",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	stdio := CollectAuthHeaders(ConnectionInfo{Env: map[string]string{"X_W17_CLIENT_ADDR": "1.1.1.1", "AUTHORIZATION": "Bearer t"}})
	if _, ok := stdio[principal.ClientAddrKey]; ok || stdio[principal.SurfaceKey] != principal.SurfaceMCP {
		t.Errorf("stdio = %v", stdio)
	}
}

// Over HTTP the auth method is told the tool's backing RPC (SetToolMethod) as
// the call's method, and the caller's address resolved behind the trusted
// proxies (SetTrustedProxies): the right-most untrusted X-Forwarded-For hop
// from a trusted peer, never a hop an untrusted peer wrote.
func TestServeStreamableHTTP_TellsTheAuthMethodTheToolMethodAndTheResolvedAddress(t *testing.T) {
	var mu sync.Mutex
	var seen []map[string]string
	auth := func(_ context.Context, c ConnectionInfo) (metadata.MD, error) {
		mu.Lock()
		seen = append(seen, CollectAuthHeaders(c))
		mu.Unlock()
		return metadata.MD{}, nil
	}
	s := NewServer("x", "1", auth)
	s.SetToolMethod("t", "/pkg.Svc/DoIt")
	s.SetTrustedProxies([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	s.AddTool("t", "d", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return s.CallUnary(ctx, req, &emptypb.Empty{}, func(context.Context, proto.Message) (proto.Message, error) {
			return &emptypb.Empty{}, nil
		})
	})
	base := startHTTP(t, s)
	if resp := callToolHTTP(t, base, "t", http.Header{"X-Forwarded-For": {"198.51.100.9, 203.0.113.5"}}); !strings.Contains(resp, `"result"`) {
		t.Fatalf("tool call failed: %s", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the auth method was never asked")
	}
	got := seen[len(seen)-1]
	if got[principal.MethodKey] != "/pkg.Svc/DoIt" {
		t.Errorf("method = %q, want the tool's RPC", got[principal.MethodKey])
	}
	if got[principal.ClientAddrKey] != "203.0.113.5" {
		t.Errorf("client address = %q, want the right-most hop the trusted proxy appended", got[principal.ClientAddrKey])
	}
}
