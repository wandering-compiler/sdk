package restgw_test

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// REV-032 Cat 4 sweep, F9: panic in a downstream handler →
// HTTP 500 + observx report + standard restgw envelope.
// Panic value never leaks to the client (Phase C principle 2).
func TestRecoverPanicMiddleware_CatchesPanic(t *testing.T) {
	// Capture log so the observx fallback (log.Printf) doesn't
	// pollute test output.
	var logBuf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(original)

	wrapped := restgw.RecoverPanicMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		panic("boom: secret internal state")
	}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/anything", nil)
	wrapped.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != "INTERNAL" {
		t.Errorf("code = %q, want INTERNAL", env.Error.Code)
	}
	if strings.Contains(env.Error.Message, "boom") || strings.Contains(env.Error.Message, "secret") {
		t.Errorf("panic value leaked to client: %q", env.Error.Message)
	}
	// Internal log SHOULD carry the panic identity for ops.
	if !strings.Contains(logBuf.String(), "PANIC GET /api/v1/anything") {
		t.Errorf("log should carry PANIC line: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "boom") {
		t.Errorf("log should carry panic value: %q", logBuf.String())
	}
}

// http.ErrAbortHandler is the standard sentinel for
// "handler aborted intentionally" — re-panic so net/http's
// own handling kicks in (silent close, no log noise).
func TestRecoverPanicMiddleware_RepanicsAbortHandler(t *testing.T) {
	wrapped := restgw.RecoverPanicMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		rec := recover()
		if rec != http.ErrAbortHandler {
			t.Errorf("expected re-panic with http.ErrAbortHandler; got %v", rec)
		}
	}()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	wrapped.ServeHTTP(w, r)
}

// No panic = pass-through.
func TestRecoverPanicMiddleware_NoPanicPassThrough(t *testing.T) {
	called := false
	wrapped := restgw.RecoverPanicMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	wrapped.ServeHTTP(rec, r)
	if !called {
		t.Error("downstream not called")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
}
