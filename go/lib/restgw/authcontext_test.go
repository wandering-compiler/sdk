package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// fromAddr is a request with one credential, as ClientAddrMiddleware leaves
// it for a caller at addr behind a trusted proxy at 10.0.0.1.
func fromAddr(t *testing.T, addr string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "10.0.0.1:443"
	r.Header.Set("Authorization", "Bearer bot-token")
	r.Header.Set("X-Forwarded-For", addr)
	var out *http.Request
	restgw.ClientAddrMiddleware([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { out = r }),
	).ServeHTTP(httptest.NewRecorder(), r)
	return out
}

// The middleware resolves the caller behind the trusted proxy, not the proxy.
func TestClientAddrMiddleware_ResolvesTheCaller(t *testing.T) {
	if got := restgw.ClientAddrFrom(fromAddr(t, "203.0.113.7").Context()); got != "203.0.113.7" {
		t.Fatalf("client addr = %q", got)
	}
	if got := restgw.ClientAddrFrom(context.Background()); got != "" {
		t.Fatalf("without the middleware = %q", got)
	}
}

// An auth result may depend on the caller's address (a machine account's
// allowlist): one made for one address is never answered to another — in
// the plain cache, the decision cache and the admin lookup cache alike.
func TestAuthCaches_KeyByClientAddress(t *testing.T) {
	var calls atomic.Int32
	inner := func(_ context.Context, r *http.Request) ([]byte, error) {
		calls.Add(1)
		return []byte(restgw.ClientAddrFrom(r.Context())), nil
	}
	for name, auth := range map[string]restgw.AuthFunc{
		"key headers": restgw.CachedAuthFuncWithKeyHeaders(inner, time.Minute, nil),
		"decision": restgw.CachedAuthFuncWithDecision(inner, time.Minute, nil, func(context.Context, *http.Request) restgw.AuthCacheDecision {
			return restgw.AuthCacheDecision{Namespace: "dpop:jkt"}
		}),
	} {
		calls.Store(0)
		a, b := fromAddr(t, "203.0.113.7"), fromAddr(t, "198.51.100.9")
		for _, r := range []*http.Request{a, a, b} {
			got, err := auth(r.Context(), r)
			if err != nil || string(got) != restgw.ClientAddrFrom(r.Context()) {
				t.Fatalf("%s: answered %q to %s", name, got, restgw.ClientAddrFrom(r.Context()))
			}
		}
		if calls.Load() != 2 {
			t.Errorf("%s: %d inner calls, want 2 (one per address)", name, calls.Load())
		}
	}

	c := restgw.NewAuthLookupCache(time.Minute)
	c.Put(fromAddr(t, "203.0.113.7"), &w17pb.AdminSignInOption{Label: "allowed"})
	if c.Get(fromAddr(t, "198.51.100.9"), &w17pb.AdminSignInOption{}) {
		t.Error("the admin lookup cache answered another address")
	}
	if !c.Get(fromAddr(t, "203.0.113.7"), &w17pb.AdminSignInOption{}) {
		t.Error("the admin lookup cache missed the same address")
	}
}

func TestAuthMethod_RoundTrips(t *testing.T) {
	ctx := restgw.WithAuthMethod(context.Background(), "/pkg.Svc/M")
	if got := restgw.AuthMethodFrom(ctx); got != "/pkg.Svc/M" {
		t.Fatalf("method = %q", got)
	}
}
