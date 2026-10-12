package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// A stream re-authenticated through the auth cache ends once a revoked
// credential's cache entry expires: the revocation lands within
// max(re-auth interval, cache TTL), not never.
func TestWatchStreamAuth_RevocationLandsThroughTheCacheTTL(t *testing.T) {
	var revoked atomic.Bool
	inner := restgw.AuthFunc(func(context.Context, *http.Request) ([]byte, error) {
		if revoked.Load() {
			return nil, status.Error(codes.Unauthenticated, "revoked")
		}
		return []byte("principal"), nil
	})
	const ttl, interval = 300 * time.Millisecond, 20 * time.Millisecond
	cached := restgw.CachedAuthFunc(inner, ttl)
	r := httptest.NewRequest(http.MethodGet, "/v1/notes/live", nil)
	r.Header.Set("Authorization", "Bearer still-cached")
	if _, err := cached(context.Background(), r); err != nil {
		t.Fatalf("first auth: %v", err)
	}

	ctx, stop := restgw.WatchStreamAuth(context.Background(), cached, r, interval)
	defer stop()
	revokedAt := time.Now()
	revoked.Store(true)
	select {
	case <-ctx.Done():
		if took := time.Since(revokedAt); took > ttl+interval+500*time.Millisecond {
			t.Errorf("revocation landed after %v, want within max(interval, TTL) ≈ %v", took, ttl)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked credential kept streaming through the auth cache")
	}
}
