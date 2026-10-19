package restgw

import (
	"context"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

type clientAddrCtxKey struct{}
type authMethodCtxKey struct{}

// ClientAddrMiddleware records each request's client address (see
// [ratelimit.HTTPClientAddr]) in its context, where the auth method's
// headers ([ClientAddrFrom]) and every auth cache read it: a cached result is
// only ever reused for the address it was made for.
func ClientAddrMiddleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), clientAddrCtxKey{}, ratelimit.HTTPClientAddr(r, trusted))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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

// addrKeyed puts the request's client address in front of a cache key, so an
// entry is never answered to another address (an auth method may decide by
// address — a machine account's allowlist). The header keys are base64 (no
// '@'), so a prefixed key never collides with a plain one.
func addrKeyed(r *http.Request, key string) string {
	addr := ClientAddrFrom(r.Context())
	if addr == "" {
		return key
	}
	return "@" + strconv.Itoa(len(addr)) + ":" + addr + ":" + key
}
