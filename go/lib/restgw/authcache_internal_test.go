package restgw

import (
	"testing"
	"time"
)

// TestAuthCacheGet pins the auth-cache read arms: a missing key misses, a
// live (future-expiry) entry hits, and a stale (past-expiry) entry is
// evicted on read and misses.
func TestAuthCacheGet(t *testing.T) {
	var c authCache
	if _, ok := c.get("absent"); ok {
		t.Error("missing key must miss")
	}
	c.m.Store("live", authEntry{data: []byte("ok"), expiry: time.Now().Add(time.Hour)})
	if v, ok := c.get("live"); !ok || string(v) != "ok" {
		t.Errorf("live entry = (%q,%v), want (ok,true)", v, ok)
	}
	c.m.Store("stale", authEntry{data: []byte("old"), expiry: time.Now().Add(-time.Hour)})
	if _, ok := c.get("stale"); ok {
		t.Error("stale entry must miss (and be evicted)")
	}
	if _, present := c.m.Load("stale"); present {
		t.Error("stale entry must be deleted from the map on read")
	}
}
