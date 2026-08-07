package restgw_test

import (
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// REV-032 Cat 4 sweep, F13: empty <PREFIX>_CORS_MAX_AGE
// (operator never set it) defaults to 600 seconds —
// production-idiomatic per MDN best-practice; without
// Access-Control-Max-Age browsers fall back to ~5 seconds
// which spams preflight on every API call.
func TestCORSConfigFromEnv_DefaultMaxAge(t *testing.T) {
	cfg := restgw.CORSConfigFromEnv("X", lookup(map[string]string{
		"X_CORS_ORIGINS": "https://example.com",
	}))
	if cfg.MaxAgeSeconds != 600 {
		t.Errorf("MaxAgeSeconds = %d, want 600 (default)", cfg.MaxAgeSeconds)
	}
}

// Operators who explicitly want browser-default short cache
// set _CORS_MAX_AGE=0 — explicit value MUST be respected
// (Phase C principle: no surprise overrides of explicit
// operator config).
func TestCORSConfigFromEnv_ExplicitZeroRespected(t *testing.T) {
	cfg := restgw.CORSConfigFromEnv("X", lookup(map[string]string{
		"X_CORS_ORIGINS": "https://example.com",
		"X_CORS_MAX_AGE": "0",
	}))
	if cfg.MaxAgeSeconds != 0 {
		t.Errorf("MaxAgeSeconds = %d, want 0 (explicit operator override)", cfg.MaxAgeSeconds)
	}
}

// Explicit positive value also flows through (sanity).
func TestCORSConfigFromEnv_ExplicitPositive(t *testing.T) {
	cfg := restgw.CORSConfigFromEnv("X", lookup(map[string]string{
		"X_CORS_ORIGINS": "https://example.com",
		"X_CORS_MAX_AGE": "3600",
	}))
	if cfg.MaxAgeSeconds != 3600 {
		t.Errorf("MaxAgeSeconds = %d, want 3600", cfg.MaxAgeSeconds)
	}
}

// Empty origins → empty config; MaxAge default doesn't fire
// since the middleware short-circuits anyway.
func TestCORSConfigFromEnv_NoOriginsEmpty(t *testing.T) {
	cfg := restgw.CORSConfigFromEnv("X", lookup(map[string]string{}))
	if cfg.MaxAgeSeconds != 0 {
		t.Errorf("MaxAgeSeconds = %d, want 0 (no-origins zero value)", cfg.MaxAgeSeconds)
	}
	if len(cfg.AllowedOrigins) != 0 {
		t.Error("origins should be empty")
	}
}
