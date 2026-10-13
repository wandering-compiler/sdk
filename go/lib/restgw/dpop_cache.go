package restgw

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// CachedDPoPAuthFunc is [CachedAuthFuncWithKeyHeaders] for a surface behind a
// [DPoPGate]. The existing cache stays as it is; this is a separate one with
// one more rule set:
//
//   - A request the gate verified ([DPoPFromContext]) is keyed on its
//     credential headers AND the verified thumbprint. The `DPoP` proof header
//     itself is not keyed on (it changes every request) and does not make
//     the request uncacheable. Two keys presenting the same token never share
//     an entry, and an entry is only ever reached after the gate's checks —
//     proof, nonce, jti, binding — passed for this request.
//   - A request that carries DPoP material (a `DPoP` header, or an
//     `Authorization: DPoP` token) but was NOT verified on this request is
//     never answered from the cache and never fills it: it goes straight to
//     inner. A gate that is not mounted, or mounted behind the cache, can
//     therefore cost cache hits but never serve a cached principal to an
//     unproven request.
//   - Any other request (bearer, cookie) is cached exactly as
//     [CachedAuthFuncWithKeyHeaders] caches it, in this cache's own entries.
//
// ttl <= 0 returns inner unchanged.
func CachedDPoPAuthFunc(inner AuthFunc, ttl time.Duration, extraCredHeaders []string) AuthFunc {
	if ttl <= 0 {
		return inner
	}
	keyHeaders := canonicalHeaderSet(defaultAuthCacheKeyHeaders, extraCredHeaders)
	sortedKeyNames := sortedSetKeys(keyHeaders)
	proofHeader := http.CanonicalHeaderKey(dpop.HeaderProof)
	c := &authCache{ttl: ttl}
	return func(ctx context.Context, r *http.Request) ([]byte, error) {
		v, verified := DPoPFromContext(r.Context())
		if !verified {
			v, verified = DPoPFromContext(ctx)
		}
		var key string
		var cacheable bool
		switch {
		case verified:
			key, cacheable = hashHeadersExcept(r.Header, keyHeaders, sortedKeyNames, proofHeader)
			// The base key is base64 (no ':'), so a verified key can never
			// equal an unverified one.
			key = "dpop:" + v.Thumbprint + ":" + key
		case carriesDPoP(r):
			return inner(ctx, r)
		default:
			key, cacheable = hashHeaders(r.Header, keyHeaders, sortedKeyNames)
		}
		if !cacheable {
			return inner(ctx, r)
		}
		if data, ok := c.get(key); ok {
			return data, nil
		}
		data, err := inner(ctx, r)
		if err != nil {
			return nil, err
		}
		c.put(key, data)
		return data, nil
	}
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
