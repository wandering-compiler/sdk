package restgw

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

type clientAddrCtxKey struct{}
type requestHostCtxKey struct{}
type authMethodCtxKey struct{}

// ClientAddrMiddleware records each request's client address (see
// [ratelimit.HTTPClientAddr]) in its context, where the auth method's
// headers ([ClientAddrFrom]) and every auth cache read it: a cached result is
// only ever reused for the address it was made for. It records the host the
// request arrived on beside it ([RequestHostFrom]), under the same trust.
func ClientAddrMiddleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), clientAddrCtxKey{}, ratelimit.HTTPClientAddr(r, trusted))
			ctx = context.WithValue(ctx, requestHostCtxKey{}, requestHost(r, trusted))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestHostFrom is the host [ClientAddrMiddleware] recorded, "" without
// it.
func RequestHostFrom(ctx context.Context) string {
	s, _ := ctx.Value(requestHostCtxKey{}).(string)
	return s
}

// requestHost is the host a request arrived on, lower-case, without a port
// or a trailing dot: the first X-Forwarded-Host when the peer is a trusted
// proxy (the one that terminated the client's connection wrote it), else the
// Host header. A client writes either freely; what it buys is the choice of
// an organization it is a member of anyway (the auth plugin still requires
// the membership), the same choice W17-Org gives it.
func requestHost(r *http.Request, trusted []netip.Prefix) string {
	h := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" && peerTrusted(r, trusted) {
		h, _, _ = strings.Cut(fwd, ",")
	}
	h = strings.TrimSpace(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// peerTrusted reports whether the request's immediate peer is in trusted.
func peerTrusted(r *http.Request, trusted []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	peer = peer.Unmap()
	for _, p := range trusted {
		if p.Contains(peer) {
			return true
		}
	}
	return false
}

// ClientAddrFrom is the address [ClientAddrMiddleware] recorded, "" without
// it.
func ClientAddrFrom(ctx context.Context) string {
	s, _ := ctx.Value(clientAddrCtxKey{}).(string)
	return s
}

// WithAuthMethod records what a request calls — the backing RPC's full
// method — for the auth method to be told (principal.MethodKey). Carried in
// the context, not a header: the auth call's headers are compared across
// the request (DPoP's bind and take), and a route only knows its method once
// it is routed.
func WithAuthMethod(ctx context.Context, fullMethod string) context.Context {
	return context.WithValue(ctx, authMethodCtxKey{}, fullMethod)
}

// AuthMethodFrom is the method [WithAuthMethod] recorded, "" without it.
func AuthMethodFrom(ctx context.Context) string {
	s, _ := ctx.Value(authMethodCtxKey{}).(string)
	return s
}

// addrKeyed puts the request's client address and host in front of a cache
// key, so an entry is never answered to another address (an auth method may
// decide by address — a machine account's allowlist) or another host (an
// audience may take the organization from the host). The header keys are
// base64 (no '@' or '#'), and each prefix is length-delimited, so a prefixed
// key never collides with a plain one or with another prefix.
func addrKeyed(r *http.Request, key string) string {
	if addr := ClientAddrFrom(r.Context()); addr != "" {
		key = "@" + strconv.Itoa(len(addr)) + ":" + addr + ":" + key
	}
	if host := RequestHostFrom(r.Context()); host != "" {
		key = "#" + strconv.Itoa(len(host)) + ":" + host + ":" + key
	}
	return key
}
