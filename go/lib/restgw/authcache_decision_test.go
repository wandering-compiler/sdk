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

// The decision hook: Bypass never reads nor fills, a Namespace keeps entries
// apart (and apart from un-namespaced ones), IgnoreHeader neither keys nor
// defeats the cache, and the zero decision is CachedAuthFuncWithKeyHeaders.
func TestCachedAuthFuncWithDecision(t *testing.T) {
	var calls atomic.Int32
	inner := func(_ context.Context, r *http.Request) ([]byte, error) {
		calls.Add(1)
		return []byte("for:" + r.Header.Get("X-Ns")), nil
	}
	decide := func(_ context.Context, r *http.Request) restgw.AuthCacheDecision {
		return restgw.AuthCacheDecision{
			Bypass:       r.Header.Get("X-Bypass") != "",
			Namespace:    r.Header.Get("X-Ns"),
			IgnoreHeader: "x-volatile",
		}
	}
	cached := restgw.CachedAuthFuncWithDecision(inner, time.Minute, []string{"X-Ns", "X-Bypass"}, decide)
	req := func(ns, volatile string, bypass bool) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer tok")
		if ns != "" {
			r.Header.Set("X-Ns", ns)
		}
		if volatile != "" {
			r.Header.Set("X-Volatile", volatile)
		}
		if bypass {
			r.Header.Set("X-Bypass", "1")
		}
		return r
	}
	call := func(r *http.Request) string {
		t.Helper()
		data, err := cached(r.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	call(req("a", "1", false))
	call(req("a", "2", false)) // ignored header: still a hit
	if calls.Load() != 1 {
		t.Fatalf("IgnoreHeader defeated the cache: %d inner calls", calls.Load())
	}
	if got := call(req("b", "1", false)); got != "for:b" {
		t.Errorf("namespace b answered %q", got)
	}
	before := calls.Load()
	call(req("a", "", true))
	call(req("a", "", true))
	if calls.Load() != before+2 {
		t.Error("Bypass was answered from the cache")
	}
	off := restgw.CachedAuthFuncWithDecision(inner, 0, nil, decide)
	before = calls.Load()
	r := req("a", "", false)
	_, _ = off(r.Context(), r)
	_, _ = off(r.Context(), r)
	if calls.Load() != before+2 {
		t.Error("ttl 0 still cached")
	}
	plain := restgw.CachedAuthFuncWithDecision(inner, time.Minute, nil, nil)
	before = calls.Load()
	pr := httptest.NewRequest(http.MethodGet, "/", nil)
	pr.Header.Set("Authorization", "Bearer tok")
	_, _ = plain(pr.Context(), pr)
	_, _ = plain(pr.Context(), pr)
	if calls.Load() != before+1 {
		t.Error("nil decide does not cache like CachedAuthFuncWithKeyHeaders")
	}
}
