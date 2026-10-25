package secheaders_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/secheaders"
)

func TestMiddleware_SetsEveryDefault(t *testing.T) {
	// A bare 404 — the shape of every early return the wrap must cover.
	h := secheaders.Middleware(http.NotFoundHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	for k, want := range secheaders.Defaults {
		if got := rec.Header().Values(k); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want exactly [%q]", k, got, want)
		}
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Errorf("HSTS = %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
		t.Errorf("CSP = %q", got)
	}
}

func TestMiddleware_HandlerValueWins(t *testing.T) {
	h := secheaders.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Values("Referrer-Policy"); len(got) != 1 || got[0] != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want the handler's [no-referrer]", got)
	}
	if got := rec.Header().Values("Content-Security-Policy"); len(got) != 1 || got[0] != "default-src 'none'" {
		t.Errorf("CSP = %q, want the handler's", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff dropped beside a handler's own headers: %q", got)
	}
}
