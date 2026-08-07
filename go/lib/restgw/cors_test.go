package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// Empty AllowedOrigins returns the next handler unchanged —
// CORS is opt-in, deployments without browser frontends pay
// nothing.
func TestCORSMiddleware_DisabledWhenNoOrigins(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	h := restgw.CORSMiddleware(restgw.CORSConfig{}, next)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler should have been called")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO should be empty when CORS disabled; got %q", got)
	}
}

// Simple GET with matching origin gains ACAO + Vary headers
// + flows through to next.
func TestCORSMiddleware_SimpleRequest_MatchedOrigin(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	cfg := restgw.CORSConfig{
		AllowedOrigins: []string{"https://app.example.com", "https://other.example.com"},
	}
	h := restgw.CORSMiddleware(cfg, next)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next not called on simple request")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q, want echo of allowed origin", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary should mention Origin; got %q", rec.Header().Get("Vary"))
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("credentials default off → header should be absent; got %q", got)
	}
}

// Non-matching origin: spec says response carries no ACAO →
// browser refuses. Server still passes the request through to
// next (the spec puts enforcement on the browser, not us).
func TestCORSMiddleware_SimpleRequest_DisallowedOrigin(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	cfg := restgw.CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
	}
	h := restgw.CORSMiddleware(cfg, next)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Errorf("next should still be called on disallowed origin (browser enforces, not server)")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin should not echo ACAO; got %q", got)
	}
}

// Wildcard config (`*` in allowlist) emits literal `*` in
// the ACAO header when credentials are NOT enabled.
func TestCORSMiddleware_Wildcard_NoCredentials(t *testing.T) {
	cfg := restgw.CORSConfig{AllowedOrigins: []string{"*"}}
	h := restgw.CORSMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://anywhere.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want \"*\"", got)
	}
}

// R-restgw-2 — wildcard + credentials is FORBIDDEN open
// credentialed CORS (reflecting an arbitrary Origin with
// Access-Control-Allow-Credentials: true lets any site make
// credentialed cross-origin calls). The middleware must refuse
// the combination by dropping credentials: it keeps the open `*`
// (so non-credentialed callers still work) but emits NO
// Access-Control-Allow-Credentials header, and ACAO stays the
// bare `*` rather than reflecting the request Origin.
func TestCORSMiddleware_Wildcard_WithCredentials_DropsCredentials(t *testing.T) {
	cfg := restgw.CORSConfig{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
	}
	h := restgw.CORSMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://anywhere.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want bare \"*\" (no Origin reflection under dropped credentials)", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("ACAC = %q, want empty (credentials refused for wildcard allowlist)", got)
	}
}

// R-restgw-2 — credentials with a CONCRETE origin allowlist is the
// legitimate, safe case and must keep working: the matched origin
// is echoed and Access-Control-Allow-Credentials: true ships.
func TestCORSMiddleware_ConcreteOrigin_WithCredentials_Kept(t *testing.T) {
	cfg := restgw.CORSConfig{
		AllowedOrigins:   []string{"https://app.example.com"},
		AllowCredentials: true,
	}
	h := restgw.CORSMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q, want concrete origin echo", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("ACAC = %q, want true (credentials valid with a concrete allowlist)", got)
	}
}

// Preflight: OPTIONS + Access-Control-Request-Method header →
// 204 + Allow headers, next is NOT called.
func TestCORSMiddleware_Preflight(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	cfg := restgw.CORSConfig{
		AllowedOrigins:   []string{"https://app.example.com"},
		AllowedMethods:   []string{"GET", "POST"},
		AllowedHeaders:   []string{"Content-Type", "X-Tenant-Id"},
		AllowCredentials: true,
		MaxAgeSeconds:    600,
	}
	h := restgw.CORSMiddleware(cfg, next)

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Error("preflight should NOT dispatch to next")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Errorf("Allow-Methods = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, X-Tenant-Id" {
		t.Errorf("Allow-Headers = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Max-Age = %q, want 600", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("preflight ACAO = %q", got)
	}
}

// OPTIONS without `Access-Control-Request-Method` is NOT a
// preflight — it's a legitimate OPTIONS verb the application
// might handle (Allow header listing, etc.). Middleware must
// pass through to next instead of swallowing it.
func TestCORSMiddleware_PlainOptions_NotSwallowed(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	cfg := restgw.CORSConfig{AllowedOrigins: []string{"https://app.example.com"}}
	h := restgw.CORSMiddleware(cfg, next)

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	// No Access-Control-Request-Method header — plain OPTIONS.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Error("plain OPTIONS without ACR-Method should reach next")
	}
}

// No Origin header → middleware leaves response untouched +
// passes through. Same-origin requests don't set Origin in
// most browsers; they shouldn't gain CORS headers.
func TestCORSMiddleware_NoOriginHeader_PassesThrough(t *testing.T) {
	cfg := restgw.CORSConfig{AllowedOrigins: []string{"https://app.example.com"}}
	h := restgw.CORSMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil) // no Origin set
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("no-origin request should not get ACAO; got %q", got)
	}
}

// CORSConfigFromEnv parses the canonical env triplet — origins
// CSV with whitespace, methods/headers overrides, credentials
// flag, max-age. Empty origins → zero CORSConfig (CORS off).
func TestCORSConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"GW_CORS_ORIGINS":           " https://a.example.com , https://b.example.com ,",
		"GW_CORS_METHODS":           "GET,POST",
		"GW_CORS_HEADERS":           "Content-Type,X-Tenant-Id",
		"GW_CORS_ALLOW_CREDENTIALS": "true",
		"GW_CORS_MAX_AGE":           "600",
	}
	lookup := func(k string) string { return env[k] }

	cfg := restgw.CORSConfigFromEnv("GW", lookup)
	if len(cfg.AllowedOrigins) != 2 {
		t.Fatalf("AllowedOrigins = %v, want 2 entries", cfg.AllowedOrigins)
	}
	if cfg.AllowedOrigins[0] != "https://a.example.com" || cfg.AllowedOrigins[1] != "https://b.example.com" {
		t.Errorf("origins = %v", cfg.AllowedOrigins)
	}
	if cfg.AllowedMethods[0] != "GET" || cfg.AllowedMethods[1] != "POST" {
		t.Errorf("methods = %v", cfg.AllowedMethods)
	}
	if cfg.AllowedHeaders[0] != "Content-Type" || cfg.AllowedHeaders[1] != "X-Tenant-Id" {
		t.Errorf("headers = %v", cfg.AllowedHeaders)
	}
	if !cfg.AllowCredentials {
		t.Error("AllowCredentials should be true")
	}
	if cfg.MaxAgeSeconds != 600 {
		t.Errorf("MaxAgeSeconds = %d, want 600", cfg.MaxAgeSeconds)
	}
}

func TestCORSConfigFromEnv_EmptyOriginsZeroValue(t *testing.T) {
	cfg := restgw.CORSConfigFromEnv("GW", func(string) string { return "" })
	if len(cfg.AllowedOrigins) != 0 {
		t.Errorf("expected empty origins; got %v", cfg.AllowedOrigins)
	}
	if cfg.AllowCredentials || cfg.MaxAgeSeconds != 0 {
		t.Errorf("zero CORSConfig expected; got %+v", cfg)
	}
}
