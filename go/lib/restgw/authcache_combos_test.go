package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// countingAuth answers with the credential it was given, so a cache that
// handed one caller another's slot would show it.
func countingAuth(calls *atomic.Int32) restgw.AuthFunc {
	return func(_ context.Context, r *http.Request) ([]byte, error) {
		calls.Add(1)
		return []byte(r.Header.Get(restgw.CredentialHeader) + r.Header.Get("Authorization")), nil
	}
}

// Two capability tokens never share a cache slot: the reserved credential
// header is part of the key, so each token resolves to its own principal and
// only a repeat of the same token is served from the cache.
func TestCachedAuth_DistinctCapabilityTokensNeverShareASlot(t *testing.T) {
	var calls atomic.Int32
	auth := restgw.CachedAuthFunc(countingAuth(&calls), time.Minute)
	req := func(token string) *http.Request {
		return restgw.WithURLCredential(httptest.NewRequest(http.MethodGet, "/public/notes/live", nil), token)
	}
	a, _ := auth(context.Background(), req("cap-A"))
	b, _ := auth(context.Background(), req("cap-B"))
	if string(a) != "cap-A" || string(b) != "cap-B" {
		t.Fatalf("principals = %q, %q; a token was answered with another's slot", a, b)
	}
	if again, _ := auth(context.Background(), req("cap-A")); string(again) != "cap-A" || calls.Load() != 2 {
		t.Errorf("repeat of cap-A = %q after %d calls, want a cache hit (2 calls)", again, calls.Load())
	}
}

// An org-scoped call (W17-Org) and a promoted ticket request each make a
// full auth call every time: the answer depends on more than the
// credential, so the cache must not serve it.
func TestCachedAuth_OrgScopedAndPromotedRequestsBypassTheCache(t *testing.T) {
	for name, header := range map[string]string{"W17-Org": "org-7", "X-W17-Ticket-Promoted": "marker"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			auth := restgw.CachedAuthFunc(countingAuth(&calls), time.Minute)
			for i := 0; i < 3; i++ {
				r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)
				r.Header.Set("Authorization", "Bearer same")
				r.Header.Set(name, header)
				if _, err := auth(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			}
			if n := calls.Load(); n != 3 {
				t.Errorf("three calls with %s reached auth %d times, want 3 (bypass)", name, n)
			}
		})
	}
}
