package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
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

// The host a request arrived on: X-Forwarded-Host only from a trusted proxy,
// else Host — normalised, so a binding of `acme.example.com` matches however
// the client spelled it.
func TestClientAddrMiddleware_RecordsTheHost(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, c := range []struct {
		name, peer, host, fwd, want string
	}{
		{"the Host header", "203.0.113.7:5000", "Acme.Example.com:8443", "", "acme.example.com"},
		{"a trailing dot", "203.0.113.7:5000", "acme.example.com.", "", "acme.example.com"},
		{"a trusted proxy's X-Forwarded-Host", "10.0.0.1:443", "gateway:8080", "acme.example.com, inner.example", "acme.example.com"},
		{"an untrusted peer's X-Forwarded-Host is ignored", "203.0.113.7:5000", "acme.example.com", "evil.example.com", "acme.example.com"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.RemoteAddr = c.peer
			r.Host = c.host
			if c.fwd != "" {
				r.Header.Set("X-Forwarded-Host", c.fwd)
			}
			var got string
			restgw.ClientAddrMiddleware(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = restgw.RequestHostFrom(r.Context())
			})).ServeHTTP(httptest.NewRecorder(), r)
			if got != c.want {
				t.Errorf("host = %q, want %q", got, c.want)
			}
		})
	}
	if got := restgw.RequestHostFrom(context.Background()); got != "" {
		t.Errorf("without the middleware = %q", got)
	}
}

// fromHost is fromAddr's request arriving on host.
func fromHost(t *testing.T, host string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "203.0.113.7:5000"
	r.Host = host
	r.Header.Set("Authorization", "Bearer bot-token")
	var out *http.Request
	restgw.ClientAddrMiddleware(nil)(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { out = r }),
	).ServeHTTP(httptest.NewRecorder(), r)
	return out
}

// An auth result may depend on the host (an audience taking the organization
// from it): one made for one host is never answered to another, for the same
// caller at the same address — in every cache.
func TestAuthCaches_KeyByHost(t *testing.T) {
	var calls atomic.Int32
	inner := func(_ context.Context, r *http.Request) ([]byte, error) {
		calls.Add(1)
		return []byte(restgw.RequestHostFrom(r.Context())), nil
	}
	for name, auth := range map[string]restgw.AuthFunc{
		"key headers": restgw.CachedAuthFuncWithKeyHeaders(inner, time.Minute, nil),
		"decision": restgw.CachedAuthFuncWithDecision(inner, time.Minute, nil, func(context.Context, *http.Request) restgw.AuthCacheDecision {
			return restgw.AuthCacheDecision{Namespace: "dpop:jkt"}
		}),
	} {
		calls.Store(0)
		a, b := fromHost(t, "acme.example.com"), fromHost(t, "globex.example.com")
		for _, r := range []*http.Request{a, a, b} {
			got, err := auth(r.Context(), r)
			if err != nil || string(got) != restgw.RequestHostFrom(r.Context()) {
				t.Fatalf("%s: answered %q to %s", name, got, restgw.RequestHostFrom(r.Context()))
			}
		}
		if calls.Load() != 2 {
			t.Errorf("%s: %d inner calls, want 2 (one per host)", name, calls.Load())
		}
	}

	c := restgw.NewAuthLookupCache(time.Minute)
	c.Put(fromHost(t, "acme.example.com"), &w17pb.AdminSignInOption{Label: "allowed"})
	if c.Get(fromHost(t, "globex.example.com"), &w17pb.AdminSignInOption{}) {
		t.Error("the admin lookup cache answered another host")
	}
	if !c.Get(fromHost(t, "acme.example.com"), &w17pb.AdminSignInOption{}) {
		t.Error("the admin lookup cache missed the same host")
	}
}

// An audience gateway's context reaches the backend on every call — the
// constants and, inside ClientAddrMiddleware, the host — under the
// principal keys, whatever the client sent.
func TestAudienceContextMiddleware(t *testing.T) {
	var got metadata.MD
	h := restgw.ClientAddrMiddleware(nil)(restgw.AudienceContextMiddleware(map[string]string{
		principal.AudienceKey:        "client",
		principal.MembershipKindsKey: "client",
		principal.TenantFromKey:      "host,default",
	}, true)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = metadata.FromOutgoingContext(r.Context())
	})))
	r := httptest.NewRequest(http.MethodPost, "/auth/register", nil)
	r.Host = "Acme.Example.com"
	h.ServeHTTP(httptest.NewRecorder(), r)
	for k, want := range map[string]string{
		principal.AudienceKey: "client", principal.MembershipKindsKey: "client",
		principal.TenantFromKey: "host,default", principal.HostKey: "acme.example.com",
	} {
		if v := got.Get(k); len(v) != 1 || v[0] != want {
			t.Errorf("%s = %v, want %q", k, v, want)
		}
	}

	got = nil
	restgw.AudienceContextMiddleware(map[string]string{principal.AudienceKey: "staff"}, false)(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = metadata.FromOutgoingContext(r.Context()) }),
	).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if v := got.Get(principal.HostKey); len(v) != 0 {
		t.Errorf("a gateway that does not read the host stamped one: %v", v)
	}
}
