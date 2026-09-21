package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// The admin middleware runs an identity lookup on EVERY request. With a
// remote identity source that is a cross-service hop per click, which is
// what this cache exists to remove — and every property below is one a
// wrong cache gets wrong in a way nobody sees until it matters.

func cacheReq(bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin/api/list/Users", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	return r
}

// identity is a stand-in for the AuthResp shape: something with a field
// a handler reads and could write through.
func identity(user string) *w17pb.AdminSignInOption {
	return &w17pb.AdminSignInOption{Label: user, StartUrl: "/x"}
}

func TestAuthLookupCache_HitReturnsWhatWasStored(t *testing.T) {
	c := restgw.NewAuthLookupCache(time.Minute)
	r := cacheReq("alice-token")

	got := &w17pb.AdminSignInOption{}
	if c.Get(r, got) {
		t.Fatal("empty cache must miss")
	}
	c.Put(r, identity("alice"))
	if !c.Get(r, got) {
		t.Fatal("the same credential must hit")
	}
	if got.GetLabel() != "alice" {
		t.Errorf("got %q, want alice", got.GetLabel())
	}
}

// TestAuthLookupCache_TwoCredentialsNeverShareASlot — the failure this
// must never have: one operator served another's identity.
func TestAuthLookupCache_TwoCredentialsNeverShareASlot(t *testing.T) {
	c := restgw.NewAuthLookupCache(time.Minute)
	c.Put(cacheReq("alice-token"), identity("alice"))
	c.Put(cacheReq("bob-token"), identity("bob"))

	got := &w17pb.AdminSignInOption{}
	if !c.Get(cacheReq("bob-token"), got) || got.GetLabel() != "bob" {
		t.Errorf("bob's credential returned %q", got.GetLabel())
	}
	if !c.Get(cacheReq("alice-token"), got) || got.GetLabel() != "alice" {
		t.Errorf("alice's credential returned %q", got.GetLabel())
	}
}

// TestAuthLookupCache_HitsDoNotAlias — a mutation by one reader must not
// reach the next.
//
// ⚠️ What this pins is the API SHAPE, not a branch. `Get` fills a
// message the CALLER owns and the cache stores bytes, so a shared
// pointer has no way to exist — the property is structural and a small
// edit cannot break it (measured: making the unmarshal a merge leaves
// this green). It is here so the contract is written down and a future
// redesign that returns a cached pointer has to delete an explicit
// statement rather than quietly change behaviour.
//
// The cache that came before this one DID hand out its backing array,
// under a read-only contract stated in a comment and enforced by
// nothing — see authCache's own note. That is the mistake this shape
// makes impossible.
func TestAuthLookupCache_HitsDoNotAlias(t *testing.T) {
	c := restgw.NewAuthLookupCache(time.Minute)
	r := cacheReq("alice-token")
	c.Put(r, identity("alice"))

	first := &w17pb.AdminSignInOption{}
	if !c.Get(r, first) {
		t.Fatal("expected a hit")
	}
	first.Label = "mallory" // a consumer mutating what it was handed

	second := &w17pb.AdminSignInOption{}
	if !c.Get(r, second) {
		t.Fatal("expected a hit")
	}
	if second.GetLabel() != "alice" {
		t.Errorf("the cached identity was rewritten by an earlier reader: got %q", second.GetLabel())
	}
}

// TestAuthLookupCache_Expires — the staleness window is the whole cost
// of this cache, so it has to actually end.
func TestAuthLookupCache_Expires(t *testing.T) {
	c := restgw.NewAuthLookupCache(20 * time.Millisecond)
	r := cacheReq("alice-token")
	c.Put(r, identity("alice"))

	got := &w17pb.AdminSignInOption{}
	if !c.Get(r, got) {
		t.Fatal("expected a hit before the TTL")
	}
	time.Sleep(40 * time.Millisecond)
	if c.Get(r, got) {
		t.Error("the entry outlived its TTL — a revoked session would keep working indefinitely")
	}
}

// TestAuthLookupCache_OffByDefault — a nil cache is a working cache
// that stores nothing, so the caller needs no conditional and caching
// stays opt-in.
func TestAuthLookupCache_OffByDefault(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		c := restgw.NewAuthLookupCache(ttl)
		if c != nil {
			t.Fatalf("ttl %v must disable the cache", ttl)
		}
		// And every method tolerates the nil.
		c.Put(cacheReq("x"), identity("x"))
		if c.Get(cacheReq("x"), &w17pb.AdminSignInOption{}) {
			t.Error("a disabled cache must never hit")
		}
	}
}

// TestAuthLookupCache_UncredentialedRequestIsNotCached — a request with
// no recognised credential header has no key that identifies a
// principal, so caching it would serve one answer to everybody.
func TestAuthLookupCache_UncredentialedRequestIsNotCached(t *testing.T) {
	c := restgw.NewAuthLookupCache(time.Minute)
	r := httptest.NewRequest(http.MethodGet, "/admin/api/whoami", nil)
	c.Put(r, identity("nobody"))
	if c.Get(r, &w17pb.AdminSignInOption{}) {
		t.Error("a request with no credential header must not be cached")
	}
}
