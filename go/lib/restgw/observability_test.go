package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// REV-032 Cat 4 sweep, F1: ObservabilityMiddleware combines
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

// B40-restgw-1: ObservabilityMiddleware (the middleware generated gateways
// actually wire) must validate the client X-Request-ID like RequestIDMiddleware
// does — a special-char / overlong id enables log-forging (restgw-sec-4) once it
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
