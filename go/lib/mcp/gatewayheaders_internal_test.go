package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// The gateway's headers reach the auth method on every transport path, keyed
// as the backend reads them, and cannot replace a credential or what the
// server stamps itself.
func TestSetGatewayHeaders(t *testing.T) {
	s := &Server{}
	in := map[string]string{
		"X-W17-Membership-Kinds": "client",
		"x-w17-audience":         "client",
		"authorization":          "Bearer forged",
		principal.SurfaceKey:     "rest",
	}
	s.SetGatewayHeaders(in)
	in["x-w17-audience"] = "staff" // the caller's map is not shared

	for name, conn := range map[string]ConnectionInfo{
		"stdio": s.connInfo(context.Background()),
		"http":  s.connInfo(context.WithValue(context.Background(), httpHeadersCtxKey{}, http.Header{"Authorization": []string{"Bearer real"}})),
	} {
		got := CollectAuthHeaders(conn)
		if got["x-w17-membership-kinds"] != "client" || got["x-w17-audience"] != "client" {
			t.Errorf("%s: gateway headers = %v", name, got)
		}
		if got["authorization"] == "Bearer forged" {
			t.Errorf("%s: a gateway header replaced the credential", name)
		}
		if got[principal.SurfaceKey] != principal.SurfaceMCP {
			t.Errorf("%s: surface = %q, want mcp", name, got[principal.SurfaceKey])
		}
	}
}
