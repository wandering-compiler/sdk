package restgw_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// G3i3-GW-B: HealthHandler returns 200 + JSON body
// `{"status":"ok"}`. Pinning the wire shape so k8s probes /
// load balancer health checks have a stable contract.
func TestHealthHandler_OKResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	restgw.HealthHandler().ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

// G3i3-GW-B: handler doesn't care about the request method —
// k8s probes default to GET, but cluster ingresses sometimes
// fire HEAD on health paths. Both should respond OK rather
// than 405.
func TestHealthHandler_AnyMethodOK(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/healthz", nil)
		restgw.HealthHandler().ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("method %s: status = %d, want 200", method, rec.Code)
		}
	}
}
