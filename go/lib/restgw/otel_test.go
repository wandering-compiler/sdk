package restgw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// REV-031 Phase C-6: OTel boot moved to lib/observx; restgw
// keeps only the HTTP-side surface — middleware wrap +
// /metrics listener. The OTelConfig / OTelConfigFromEnv /
// InitOTel surface is gone.

// OTelMiddleware always wraps via otelhttp; when no exporter
// is configured the global TracerProvider is the SDK noop and
// the wrap is near-zero-cost. Functional check: downstream
// handler still fires + response passes through.
func TestOTelMiddleware_NoopPassThrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	wrapped := restgw.OTelMiddleware(next, "test-service")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	wrapped.ServeHTTP(rec, r)

	if !called {
		t.Error("downstream handler not called")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418 (downstream's response)", rec.Code)
	}
}

// G3i3-GW-C: MetricsHandler returns a non-nil handler that
// responds with the Prometheus exposition format. Even
// without observx EnableMetrics the handler serves the
// global Go-runtime collectors — still a valid response.
func TestMetricsHandler_NotNil(t *testing.T) {
	if restgw.MetricsHandler() == nil {
		t.Fatal("MetricsHandler() returned nil")
	}
}

// G3i3-GW-C: MountMetricsListener with empty port returns
// empty addr (off path) — no goroutine spawned, no listen
// error.
func TestMountMetricsListener_EmptyPortOff(t *testing.T) {
	addr := restgw.MountMetricsListener("GW", func(string) string { return "" }, func(string, ...any) {})
	if addr != "" {
		t.Errorf("addr = %q, want empty (off path)", addr)
	}
}
