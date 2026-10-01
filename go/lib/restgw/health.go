// Health endpoint for generated gateways (G3i3-GW-B). Always-
// on liveness probe independent of any backend service —
// gateway responding to /healthz IS the health signal.
// Backend reachability is a separate concern; consumers who
// want end-to-end health bolt on a custom probe via a
// lightweight backend RPC.
//
// The handler itself lives in core/healthz since every HTTP
// listener of a generated binary (admin, MCP) mounts it and the
// `<binary> health` probe asks for it — this name stays because
// the generated REST serve.go calls it.

package restgw

import (
	"net/http"

	"github.com/wandering-compiler/sdk/go/core/healthz"
)

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
	return healthz.Handler()
}
