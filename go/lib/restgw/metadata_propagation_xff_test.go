package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TestMetadataPropagation_XForwardedForIsTheResolvedClient: a gateway
// that propagates X-Forwarded-For handed the backend the header as the
// CLIENT wrote it, and the auth plugin records the first hop as the
// device's and the sign-in's IP — so `curl -H 'X-Forwarded-For: 1.1.1.1'`
// forged the address in every session list and sign-in history. The
// value forwarded is the address ClientAddrMiddleware resolved through
// the trusted proxies, the one the rate limits already count.
func TestMetadataPropagation_XForwardedForIsTheResolvedClient(t *testing.T) {
	proxy := netip.MustParsePrefix("10.0.0.0/8")
	cases := []struct {
		name    string
		trusted []netip.Prefix
		peer    string
		xff     string
		want    string
	}{
		{"direct client forging the header", nil, "203.0.113.7:4000", "1.1.1.1", "203.0.113.7"},
		{"behind a trusted proxy", []netip.Prefix{proxy}, "10.1.2.3:4000", "1.1.1.1, 198.51.100.9", "198.51.100.9"},
		{"direct client, no header", nil, "203.0.113.7:4000", "", "203.0.113.7"},
	}
	for _, c := range cases {
		var got []string
		h := restgw.MetadataPropagationMiddleware([]string{"X-Forwarded-For", "X-Request-Id"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			md, _ := metadata.FromOutgoingContext(r.Context())
			got = md.Get("x-forwarded-for")
		}))
		h = restgw.ClientAddrMiddleware(c.trusted)(h)
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = c.peer
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: forwarded x-forwarded-for = %q, want [%q]", c.name, got, c.want)
		}
	}
}
