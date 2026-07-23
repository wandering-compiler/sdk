package restgw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

func TestRequestIDConfigFromEnv_Defaults(t *testing.T) {
	cfg := restgw.RequestIDConfigFromEnv("X", func(string) string { return "" })
	if cfg.Header != "X-Request-ID" {
		t.Errorf("Header = %q, want \"X-Request-ID\"", cfg.Header)
	}
	if cfg.Disabled {
		t.Errorf("Disabled should default false")
	}
}

func TestRequestIDConfigFromEnv_HeaderOverride(t *testing.T) {
	env := map[string]string{"X_REQUEST_ID_HEADER": "X-Trace-ID"}
	cfg := restgw.RequestIDConfigFromEnv("X", lookup(env))
	if cfg.Header != "X-Trace-ID" {
		t.Errorf("Header = %q, want \"X-Trace-ID\"", cfg.Header)
	}
}

func TestRequestIDConfigFromEnv_Disabled(t *testing.T) {
	env := map[string]string{"X_REQUEST_ID_DISABLED": "true"}
	cfg := restgw.RequestIDConfigFromEnv("X", lookup(env))
	if !cfg.Disabled {
		t.Errorf("Disabled should be true when env=\"true\"")
	}
}

func TestRequestIDMiddleware_GeneratesWhenMissing(t *testing.T) {
	var seen string
	h := restgw.RequestIDMiddleware(restgw.RequestIDConfigFromEnv("X", func(string) string { return "" }),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = restgw.RequestIDFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if seen == "" {
		t.Fatal("handler should have observed a generated request ID in ctx")
	}
	if _, err := uuid.Parse(seen); err != nil {
		t.Errorf("generated ID is not a parseable UUID: %q (%v)", seen, err)
	}
	if got := rr.Header().Get("X-Request-ID"); got != seen {
		t.Errorf("response header X-Request-ID = %q, want %q (matching ctx)", got, seen)
	}
}

func TestRequestIDMiddleware_PropagatesIncoming(t *testing.T) {
	const incoming = "client-supplied-id-123"
	var seen string
	h := restgw.RequestIDMiddleware(restgw.RequestIDConfigFromEnv("X", func(string) string { return "" }),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = restgw.RequestIDFromContext(r.Context())
		}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-ID", incoming)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen != incoming {
		t.Errorf("ctx ID = %q, want incoming %q", seen, incoming)
	}
	if got := rr.Header().Get("X-Request-ID"); got != incoming {
		t.Errorf("response header = %q, want incoming %q", got, incoming)
	}
}

func TestRequestIDMiddleware_RespectsCustomHeader(t *testing.T) {
	var seen string
	cfg := restgw.RequestIDConfig{Header: "X-Trace-ID"}
	h := restgw.RequestIDMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = restgw.RequestIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Trace-ID", "trace-abc")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen != "trace-abc" {
		t.Errorf("custom header path: ctx ID = %q, want \"trace-abc\"", seen)
	}
	if got := rr.Header().Get("X-Trace-ID"); got != "trace-abc" {
		t.Errorf("response header = %q, want \"trace-abc\"", got)
	}
	// And the default header must NOT leak through.
	if got := rr.Header().Get("X-Request-ID"); got != "" {
		t.Errorf("default header should be empty when custom header is in use; got %q", got)
	}
}

func TestRequestIDMiddleware_Disabled_PassesThrough(t *testing.T) {
	var seen string
	cfg := restgw.RequestIDConfig{Disabled: true}
	h := restgw.RequestIDMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = restgw.RequestIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen != "" {
		t.Errorf("disabled middleware should not attach a ctx value; got %q", seen)
	}
	if got := rr.Header().Get("X-Request-ID"); got != "" {
		t.Errorf("disabled middleware should not set a response header; got %q", got)
	}
}

func TestRequestIDMiddleware_TrimsBlankIncoming(t *testing.T) {
	// An incoming header that is whitespace-only should be
	// treated as missing — generated UUID replaces it.
	var seen string
	h := restgw.RequestIDMiddleware(restgw.RequestIDConfig{Header: "X-Request-ID"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = restgw.RequestIDFromContext(r.Context())
		}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-ID", "   ")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen == "" || seen == "   " {
		t.Errorf("blank incoming should be replaced; got %q", seen)
	}
	if _, err := uuid.Parse(seen); err != nil {
		t.Errorf("replacement should be a UUID; got %q", seen)
	}
}

func TestRequestIDFromContext_NilCtxReturnsEmpty(t *testing.T) {
	// Defensive: callers might invoke the helper before the
	// middleware runs (e.g. background workers) — return empty
	// instead of panicking on a nil ctx.
	if got := restgw.RequestIDFromContext(context.TODO()); got != "" {
		t.Errorf("nil ctx: got %q, want empty", got)
	}
}
