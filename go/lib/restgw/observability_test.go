package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// ObservabilityMiddleware combines
// otelhttp wrap + RequestID + span attribute. The
// "request_id on span" property is observable only when an
// SDK tracer is wired (otherwise span.IsRecording is false);
// the simpler smoke is end-to-end behaviour:
//
//   - Header echoed back on the response
//   - ctx carries the ID for downstream lookup
//   - Generated UUID when header missing
//   - Disabled flag short-circuits

func TestObservabilityMiddleware_EchoesIncomingID(t *testing.T) {
	var seenID string
	wrapped := restgw.ObservabilityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = restgw.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}), "test-service", restgw.RequestIDConfig{})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "fixed-id-12345")
	wrapped.ServeHTTP(rec, r)

	if rec.Header().Get("X-Request-ID") != "fixed-id-12345" {
		t.Errorf("response header = %q, want fixed-id-12345", rec.Header().Get("X-Request-ID"))
	}
	if seenID != "fixed-id-12345" {
		t.Errorf("ctx ID = %q, want fixed-id-12345", seenID)
	}
}

// ObservabilityMiddleware (the middleware generated gateways
// actually wire) must validate the client X-Request-ID like RequestIDMiddleware
// does — a special-char / overlong id enables log-forging once it
// reaches RequestIDFromContext and any downstream log line. A malformed id must
// be replaced with a fresh UUID, not echoed/correlated as-is.
func TestObservabilityMiddleware_RejectsMalformedRequestID_B40(t *testing.T) {
	var seenID string
	wrapped := restgw.ObservabilityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = restgw.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}), "test-service", restgw.RequestIDConfig{})

	const bad = "evil id with spaces; forged-line"
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", bad)
	wrapped.ServeHTTP(rec, r)
	if seenID == bad {
		t.Errorf("ctx ID = %q — a malformed client request-id must be rejected (UUID)", seenID)
	}
	if rec.Header().Get("X-Request-ID") == bad {
		t.Errorf("response echoed the malformed client request-id %q", bad)
	}

	// A well-formed id still passes through untouched.
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("X-Request-ID", "valid-id.123")
	wrapped.ServeHTTP(rec2, r2)
	if seenID != "valid-id.123" {
		t.Errorf("valid id = %q, want valid-id.123", seenID)
	}
}

func TestObservabilityMiddleware_GeneratesIDWhenMissing(t *testing.T) {
	var seenID string
	wrapped := restgw.ObservabilityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = restgw.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}), "test-service", restgw.RequestIDConfig{})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	wrapped.ServeHTTP(rec, r)

	hdr := rec.Header().Get("X-Request-ID")
	if hdr == "" {
		t.Error("response header missing — should be generated UUID")
	}
	if seenID == "" {
		t.Error("ctx ID empty — should be generated UUID")
	}
	if hdr != seenID {
		t.Errorf("header %q != ctx %q — should match", hdr, seenID)
	}
	// UUIDv4 form sanity check (don't pin format strictly).
	if len(seenID) < 32 || !strings.Contains(seenID, "-") {
		t.Errorf("generated ID doesn't look like a UUID: %q", seenID)
	}
}

func TestObservabilityMiddleware_DisabledSkipsRequestID(t *testing.T) {
	var seenID string
	wrapped := restgw.ObservabilityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = restgw.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}), "test-service", restgw.RequestIDConfig{Disabled: true})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "should-be-ignored")
	wrapped.ServeHTTP(rec, r)

	if rec.Header().Get("X-Request-ID") != "" {
		t.Errorf("disabled mode should not echo header; got %q", rec.Header().Get("X-Request-ID"))
	}
	if seenID != "" {
		t.Errorf("disabled mode should not attach ctx ID; got %q", seenID)
	}
}

func TestObservabilityMiddleware_CustomHeader(t *testing.T) {
	var seenID string
	wrapped := restgw.ObservabilityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = restgw.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}), "test-service", restgw.RequestIDConfig{Header: "X-Trace-Tag"})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Trace-Tag", "custom-id")
	wrapped.ServeHTTP(rec, r)

	if rec.Header().Get("X-Trace-Tag") != "custom-id" {
		t.Errorf("custom header not echoed; got %q", rec.Header().Get("X-Trace-Tag"))
	}
	if seenID != "custom-id" {
		t.Errorf("ctx ID = %q, want custom-id", seenID)
	}
}

// With the id disabled the fronting layer owns it, so a well-formed id is
// handed on exactly as received — but header propagation forwards that header
// to the backend, so a malformed or repeated client value must not ride along
// past the shape check the enabled mode applies. Both middlewares, same rule.
func TestRequestID_DisabledHandsOnOnlyAWellFormedHeader(t *testing.T) {
	mws := map[string]func(http.Handler) http.Handler{
		"ObservabilityMiddleware": func(h http.Handler) http.Handler {
			return restgw.ObservabilityMiddleware(h, "test-service", restgw.RequestIDConfig{Disabled: true})
		},
		"RequestIDMiddleware": func(h http.Handler) http.Handler {
			return restgw.RequestIDMiddleware(restgw.RequestIDConfig{Disabled: true}, h)
		},
	}
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"absent", nil, nil},
		{"fronting layer id", []string{"edge-7f3a.c9:01"}, []string{"edge-7f3a.c9:01"}},
		{"forged log line", []string{"ok\n2026-01-01 level=error user=admin"}, nil},
		{"over-long", []string{strings.Repeat("a", 129)}, nil},
		{"repeated", []string{"a", "b"}, nil},
	}
	for name, mw := range mws {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				var seen []string
				var seenID string
				h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen = r.Header.Values("X-Request-ID")
					seenID = restgw.RequestIDFromContext(r.Context())
				}))
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				for _, v := range tc.in {
					r.Header.Add("X-Request-ID", v)
				}
				before := append([]string(nil), r.Header.Values("X-Request-ID")...)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				if len(seen) != len(tc.want) || (len(seen) > 0 && seen[0] != tc.want[0]) {
					t.Errorf("handed on %q, want %q", seen, tc.want)
				}
				if got := r.Header.Values("X-Request-ID"); len(got) != len(before) {
					t.Errorf("caller's header map modified: %q, was %q", got, before)
				}
				if rec.Header().Get("X-Request-ID") != "" || seenID != "" {
					t.Errorf("disabled mode echoed %q / attached %q", rec.Header().Get("X-Request-ID"), seenID)
				}
			})
		}
	}
}
