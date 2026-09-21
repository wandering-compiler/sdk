package restgw

import (
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"
)

// AuthLookupCache is a TTL cache for a TYPED per-request identity
// lookup — the shape the admin bundle's middleware runs on every
// request, where [CachedAuthFunc]'s `[]byte` contract does not fit.
//
// # Why it stores bytes anyway
//
// The value is the MARSHALLED response, unmarshalled fresh on each hit.
// That is not indirection for its own sake: a cached proto handed to
// two concurrent requests is one message two handlers may write
// through, and the identity of every request holding it changes
// underneath them. [authCache] learned that the hard way and clones its
// payload for the same reason. Marshalling makes the hazard impossible
// rather than forbidden, and one unmarshal is far cheaper than the gRPC
// round trip a hit avoids.
//
// # The key rule is the same one, deliberately
//
// Keys derive from the request's credential-bearing headers through
// exactly [CachedAuthFunc]'s machinery, which means the constraint
// documented there holds here too: a request carrying a header outside
// the known set is NOT cached, so two principals can never share a
// slot. One rule, one implementation — a second cache with its own key
// derivation is how the two drift until one of them leaks an identity.
type AuthLookupCache struct {
	c          *authCache
	keyHeaders map[string]struct{}
	sorted     []string
}

// NewAuthLookupCache returns a cache with the given TTL, or nil when
// the TTL is zero or negative.
//
// Nil is a working value: every method tolerates a nil receiver, so the
// caller writes no conditional and caching-off is the same code path
// with one map lookup fewer. Off is also the DEFAULT — a cache on an
// identity lookup means a revoked session keeps working for up to the
// TTL, and that is an operator's decision to make with a number they
// chose, not one a generator makes for them.
func NewAuthLookupCache(ttl time.Duration) *AuthLookupCache {
	if ttl <= 0 {
		return nil
	}
	keyHeaders := canonicalHeaderSet(defaultAuthCacheKeyHeaders, nil)
	return &AuthLookupCache{
		c:          &authCache{ttl: ttl},
		keyHeaders: keyHeaders,
		sorted:     sortedSetKeys(keyHeaders),
	}
}

// Get fills `into` from the cache and reports whether it hit.
func (c *AuthLookupCache) Get(r *http.Request, into proto.Message) bool {
	if c == nil || into == nil {
		return false
	}
	key, cacheable := hashHeaders(r.Header, c.keyHeaders, c.sorted)
	if !cacheable {
		return false
	}
	raw, ok := c.c.get(key)
	if !ok {
		return false
	}
	if err := proto.Unmarshal(raw, into); err != nil {
		// A stored value that will not decode is this cache's own bug,
		// not the caller's problem: drop it and answer "miss" so the
		// request goes to the real lookup and the user notices nothing.
		c.c.drop(key)
		return false
	}
	return true
}

// Put stores a successful lookup.
//
// Only successes: an error from the lookup is never cached, so an auth
// service that blips does not lock every holder of a valid session out
// for the length of the TTL.
func (c *AuthLookupCache) Put(r *http.Request, msg proto.Message) {
	if c == nil || msg == nil {
		return
	}
	key, cacheable := hashHeaders(r.Header, c.keyHeaders, c.sorted)
	if !cacheable {
		return
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		return
	}
	c.c.put(key, raw)
}
