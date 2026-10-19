package mcp

import (
	"net/http"
	"testing"

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
