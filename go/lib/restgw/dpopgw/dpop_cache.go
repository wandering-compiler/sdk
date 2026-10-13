package dpopgw

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// CachedAuthFunc is restgw.CachedAuthFuncWithKeyHeaders for a surface behind
// a [Gate], built on restgw.CachedAuthFuncWithDecision. restgw's own cache
// stays as it is; this is a separate one with one more rule set:
//
//   - A request the gate verified ([FromContext]) is keyed on its credential
//     headers AND the verified thumbprint. The `DPoP` proof header itself is
//     not keyed on (it changes every request) and does not make the request
//     uncacheable. Two keys presenting the same token never share an entry,
//     and an entry is only ever reached after the gate's checks — proof,
//     nonce, jti, binding — passed for this request.
//   - A request that carries DPoP material (a `DPoP` header, or an
//     `Authorization: DPoP` token) but was NOT verified on this request is
//     never answered from the cache and never fills it: it goes straight to
//     inner. A gate that is not mounted, or mounted behind the cache, can
//     therefore cost cache hits but never serve a cached principal to an
//     unproven request.
//   - Any other request (bearer, cookie) is cached exactly as restgw caches
//     it, in this cache's own entries.
//
// ttl <= 0 returns inner unchanged.
func CachedAuthFunc(inner restgw.AuthFunc, ttl time.Duration, extraCredHeaders []string) restgw.AuthFunc {
	return restgw.CachedAuthFuncWithDecision(inner, ttl, extraCredHeaders, func(ctx context.Context, r *http.Request) restgw.AuthCacheDecision {
		v, verified := FromContext(r.Context())
		if !verified {
			v, verified = FromContext(ctx)
		}
		switch {
		case verified:
			return restgw.AuthCacheDecision{Namespace: "dpop:" + v.Thumbprint, IgnoreHeader: dpop.HeaderProof}
		case carriesDPoP(r):
			return restgw.AuthCacheDecision{Bypass: true}
		}
		return restgw.AuthCacheDecision{}
	})
}

// carriesDPoP reports whether r presents any DPoP material: a proof header,
// or an Authorization header with the DPoP scheme (with or without a token).
func carriesDPoP(r *http.Request) bool {
	if len(r.Header.Values(dpop.HeaderProof)) > 0 {
		return true
	}
	for _, a := range r.Header.Values("Authorization") {
		if isDPoPScheme(a) {
			return true
		}
	}
	return false
}

// isDPoPScheme reports whether an Authorization value uses the DPoP scheme,
// with or without a token.
func isDPoPScheme(authorization string) bool {
	scheme, _, _ := strings.Cut(strings.TrimSpace(authorization), " ")
	return strings.EqualFold(scheme, dpop.AuthScheme)
}
