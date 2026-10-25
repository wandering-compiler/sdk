package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// The gateway's headers reach the auth method on every transport path —
// stdio, HTTP, and the OAuth-protected resource — keyed as the backend reads
// them, and a client's own copy never wins.
func TestSetGatewayHeaders_ReachTheAuthMethod(t *testing.T) {
	gw := map[string]string{"X-W17-Membership-Kinds": "client", principal.AudienceKey: "client", principal.TenantFromKey: "host,default", principal.ProductKey: "finplatform"}
	want := func(t *testing.T, path string, got map[string]string) {
		t.Helper()
		if got[principal.MembershipKindsKey] != "client" || got[principal.AudienceKey] != "client" || got[principal.TenantFromKey] != "host,default" || got[principal.ProductKey] != "finplatform" {
			t.Errorf("%s: auth headers = %v", path, got)
		}
		if got[principal.SurfaceKey] != principal.SurfaceMCP {
			t.Errorf("%s: surface = %q, want mcp", path, got[principal.SurfaceKey])
		}
	}

	s := &Server{}
	s.SetGatewayHeaders(gw)
	gw[principal.AudienceKey] = "staff" // the caller's map is not shared
	want(t, "stdio", CollectAuthHeaders(s.connInfo(context.Background())))
	clientCopy := http.Header{"Authorization": []string{"Bearer t"}, "X-W17-Audience": []string{"staff"}}
	want(t, "http", CollectAuthHeaders(s.connInfo(context.WithValue(context.Background(), httpHeadersCtxKey{}, clientCopy))))

	var seen map[string]string
	ps, h := protectedServer(t, func(_ context.Context, conn ConnectionInfo) ([]int32, error) {
		seen = CollectAuthHeaders(conn)
		return []int32{1}, nil
	})
	ps.SetGatewayHeaders(map[string]string{principal.AudienceKey: "client", principal.MembershipKindsKey: "client", principal.TenantFromKey: "host,default", principal.ProductKey: "finplatform"})
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer t")
	r.Header.Set("X-W17-Membership-Kinds", "staff")
	r.Header.Set("X-W17-Tenant-From", "default")
	h.ServeHTTP(httptest.NewRecorder(), r)
	want(t, "oauth resource", seen)
}

// Anything but the audience keys, or one key twice, is a defect in the
// caller: it panics at setup rather than being dropped or winning at random.
func TestSetGatewayHeaders_RefusesOtherKeys(t *testing.T) {
	for name, h := range map[string]map[string]string{
		"credential slot": {"x-w17-credential": "c"},
		"principal":       {"x-w17-user": "u"},
		"stamped":         {principal.SurfaceKey: "rest"},
		"the host":        {principal.HostKey: "acme.example.com"},
		"twice":           {"x-w17-audience": "a", "X-W17-Audience": "b"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: %v accepted", name, h)
				}
			}()
			(&Server{}).SetGatewayHeaders(h)
		}()
	}
}
