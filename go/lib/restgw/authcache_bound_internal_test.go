package restgw

import (
	"fmt"
	"testing"
	"time"
)

// The T3-7 pass #7 (C-F8) guards for the auth cache's SIZE.
//
// The cache had no sweeper and no cap: an entry was reclaimed only when a
// later `get` on the SAME key found it expired. Anonymous stuffing could not
// grow it (failures are never cached), but every mintable valid credential
// could — per-session tokens and rotating API keys are exactly that — so a
// long-lived gateway accumulated one entry per credential it ever saw, for the
// life of the process. The bound below is what makes "cache" true rather than
// "memoise forever": going over it costs a cache MISS (one inner auth call),
// never a wrong answer.

// liveEntries counts what is ACTUALLY in the map, not what the cache's own
// counter believes — a bound the bookkeeping agrees with but the map does not
// is exactly the bug this guards.
func liveEntries(c *authCache) int {
	n := 0
	c.m.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestAuthCache_BoundedUnderDistinctValidCredentials — INVARIANT: distinct
// valid credentials, all unexpired, do not grow the cache without limit.
func TestAuthCache_BoundedUnderDistinctValidCredentials(t *testing.T) {
	c := &authCache{ttl: time.Hour}
	fed := authCacheMaxEntries * 3
	for i := 0; i < fed; i++ {
		c.put(fmt.Sprintf("credential-%d", i), []byte("identity"))
	}
	if live := liveEntries(c); live > authCacheMaxEntries {
		t.Fatalf("cache holds %d entries after %d distinct credentials — bound is %d; it grows for the life of the process",
			live, fed, authCacheMaxEntries)
	}
	// The bound must not degenerate into "never cache": the most recent
	// credential is still expected to hit.
	if _, ok := c.get(fmt.Sprintf("credential-%d", fed-1)); !ok {
		t.Error("the newest credential missed — a bound that evicts everything is not a cache")
	}
}

// TestAuthCache_ReapsExpiredWithoutASameKeyGet — INVARIANT: an expired entry
// is reclaimed by the cache's own housekeeping. Before, the ONLY reaper was a
// `get` on that same key, so a credential that was never presented again sat
// in the map forever — the common case for a rotated token.
func TestAuthCache_ReapsExpiredWithoutASameKeyGet(t *testing.T) {
	c := &authCache{ttl: time.Hour}
	c.m.Store("rotated-away", authEntry{data: []byte("old"), expiry: time.Now().Add(-time.Hour)})

	// Ordinary traffic on OTHER keys — the expired one is never asked for.
	for i := 0; i <= authCacheMaxEntries; i++ {
		c.put(fmt.Sprintf("credential-%d", i), []byte("identity"))
	}

	if _, present := c.m.Load("rotated-away"); present {
		t.Fatal("an expired entry survived a full cache turnover — nothing but a get on its own key ever reaps it")
	}
}
