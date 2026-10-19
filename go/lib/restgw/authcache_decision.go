package restgw

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// AuthCacheDecision is what the decide function of
// [CachedAuthFuncWithDecision] says about one request.
type AuthCacheDecision struct {
	// Bypass sends the request straight to inner: it is never answered from
	// the cache and never stored in it.
	Bypass bool
	// Namespace, when set, keys the request apart from every other namespace
	// and from requests with none: an entry stored under one namespace is
	// never read under another. A caller that verified something about the
	// request (a proof-of-possession thumbprint, say) puts it here.
	Namespace string
	// IgnoreHeader names one request header (any case) that is neither keyed
	// on nor treated as an unknown, possibly-credentialed header — one whose
	// content the caller already verified and folded into Namespace.
	IgnoreHeader string
}

// CachedAuthFuncWithDecision is [CachedAuthFuncWithKeyHeaders] with a
// per-request decision hook, for a layer in front of the cache (such as
// lib/restgw/dpopgw) that knows more about a request than its headers say.
// Everything the decision leaves at its zero value behaves exactly as in
// [CachedAuthFuncWithKeyHeaders]; a nil decide is that function. The cache
// is its own: entries are never shared with another cache instance.
//
// ttl <= 0 returns inner unchanged.
func CachedAuthFuncWithDecision(inner AuthFunc, ttl time.Duration, extraCredHeaders []string, decide func(ctx context.Context, r *http.Request) AuthCacheDecision) AuthFunc {
	if ttl <= 0 {
		return inner
	}
	if decide == nil {
		return CachedAuthFuncWithKeyHeaders(inner, ttl, extraCredHeaders)
	}
	keyHeaders := canonicalHeaderSet(defaultAuthCacheKeyHeaders, extraCredHeaders)
	sortedKeyNames := sortedSetKeys(keyHeaders)
	c := &authCache{ttl: ttl}
	return func(ctx context.Context, r *http.Request) ([]byte, error) {
		d := decide(ctx, r)
		if d.Bypass {
			return inner(ctx, r)
		}
		ignore := ""
		if d.IgnoreHeader != "" {
			ignore = http.CanonicalHeaderKey(d.IgnoreHeader)
		}
		key, cacheable := hashHeadersExcept(r.Header, keyHeaders, sortedKeyNames, ignore)
		if !cacheable {
			return inner(ctx, r)
		}
		if d.Namespace != "" {
			// The header key is base64 (no ':'); a length-prefixed namespace
			// can therefore never collide with a plain key or another
			// namespace.
			key = strconv.Itoa(len(d.Namespace)) + ":" + d.Namespace + ":" + key
		}
		key = addrKeyed(r, key)
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
