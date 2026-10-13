package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func TestWithDPoPCORS(t *testing.T) {
	base := restgw.CORSConfig{AllowedOrigins: []string{"https://app.example.com"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	pre := func(cfg restgw.CORSConfig) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodOptions, "/v1/notes", nil)
		r.Header.Set("Origin", "https://app.example.com")
		r.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		restgw.CORSMiddleware(cfg, next).ServeHTTP(rec, r)
		return rec
	}
	simple := func(cfg restgw.CORSConfig, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/notes", nil)
		r.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		restgw.CORSMiddleware(cfg, next).ServeHTTP(rec, r)
		return rec
	}

	// Defaults unchanged: no DPoP, nothing exposed.
	if h := pre(base).Header().Get("Access-Control-Allow-Headers"); strings.Contains(h, "DPoP") {
		t.Errorf("default config allows DPoP: %q", h)
	}
	if h := simple(base, "https://app.example.com").Header().Get("Access-Control-Expose-Headers"); h != "" {
		t.Errorf("default config exposes %q", h)
	}

	cfg := restgw.WithDPoPCORS(base)
	allow := pre(cfg).Header().Get("Access-Control-Allow-Headers")
	for _, want := range []string{"DPoP", "Authorization", "Content-Type", "W17-Org"} {
		if !strings.Contains(strings.ToLower(allow), strings.ToLower(want)) {
			t.Errorf("Allow-Headers %q lacks %s", allow, want)
		}
	}
	expose := simple(cfg, "https://app.example.com").Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(expose, "DPoP-Nonce") || !strings.Contains(expose, "WWW-Authenticate") {
		t.Errorf("Expose-Headers %q", expose)
	}
	if h := simple(cfg, "https://evil.example.com").Header().Get("Access-Control-Expose-Headers"); h != "" {
		t.Errorf("a disallowed origin got Expose-Headers %q", h)
	}

	// Operator headers are kept, nothing is doubled, the input is not
	// modified, and no origins stays CORS-off.
	custom := restgw.CORSConfig{AllowedOrigins: base.AllowedOrigins, AllowedHeaders: []string{"X-Tenant", "dpop"}, ExposedHeaders: []string{"X-Total"}}
	got := restgw.WithDPoPCORS(restgw.WithDPoPCORS(custom))
	if strings.Join(got.AllowedHeaders, ",") != "X-Tenant,dpop" {
		t.Errorf("AllowedHeaders = %v", got.AllowedHeaders)
	}
	if strings.Join(got.ExposedHeaders, ",") != "X-Total,DPoP-Nonce,WWW-Authenticate" {
		t.Errorf("ExposedHeaders = %v", got.ExposedHeaders)
	}
	if len(custom.ExposedHeaders) != 1 || len(custom.AllowedHeaders) != 2 {
		t.Error("WithDPoPCORS modified its argument")
	}
	off := restgw.WithDPoPCORS(restgw.CORSConfig{})
	if rec := simple(off, "https://app.example.com"); rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Expose-Headers") != "" {
		t.Error("a config with no origins turned CORS on")
	}
}
