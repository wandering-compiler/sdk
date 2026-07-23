// Health endpoint for generated gateways (G3i3-GW-B). Always-
// on liveness probe independent of any backend service —
// gateway responding to /healthz IS the health signal.
// Backend reachability is a separate concern; consumers who
// want end-to-end health bolt on a custom probe via a
// lightweight backend RPC.
//
// Mirrors the protobridge `runtime/health.go` pattern the
// conventions doc points at. Pre-marshals at init time so
// the per-request path is one Header().Set + WriteHeader +
// Write — no allocation on the hot path.

package restgw

import (
	"encoding/json"
	"net/http"
)

// healthBody is the pre-marshaled `{"status":"ok"}` payload.
// Marshal can't fail on this trivial struct; the literal
// bytes serve as a defensive fallback if a future schema
// addition breaks the assumption.
var healthBody = func() []byte {
	body, err := json.Marshal(struct {
		Status string `json:"status"`
	}{Status: "ok"})
	if err != nil {
		return []byte(`{"status":"ok"}`)
	}
	return body
}()

// HealthHandler returns an http.HandlerFunc serving 200 OK
// with `{"status":"ok"}`. Registered by main.go on
// `GET /healthz` before any service-specific routes.
//
// The handler is intentionally trivial: gateway being able
// to respond at all is the liveness signal. No backend dial,
// no auth check, no request body parse — those would couple
// the health signal to concerns the probe doesn't care
// about (k8s liveness probe should not fail because the
// auth service is briefly unreachable).
func HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Client-disconnect mid-write is normal for probes —
		// drop the error.
		_, _ = w.Write(healthBody)
	}
}
