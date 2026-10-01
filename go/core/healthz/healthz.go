// Package healthz is the liveness endpoint every HTTP listener of a generated
// w17 binary answers, and the path the `<binary> health` probe asks.
//
// One package for both ends because they are one contract: the probe in
// service/healthcheck GETs [Path] and expects 200, and a listener that mounts
// something else — or nothing — is a container that never turns healthy. The
// REST gateway had /healthz long before anything probed it; the admin and the
// MCP listener had nothing, so a probe of either could only have failed.
//
// A leaf with no imports outside the standard library, so lib/mcp, lib/restgw
// and a generated admin can all mount it without pulling each other in.
package healthz

import "net/http"

// Path is where the endpoint lives on every HTTP listener.
const Path = "/healthz"

// body is the fixed `{"status":"ok"}` payload — a literal, so the per-request
// path is one header, one status line and one write.
var body = []byte(`{"status":"ok"}`)

// Handler serves 200 with `{"status":"ok"}`.
//
// LIVENESS, not readiness: no backend dial, no auth, no request parse. The
// process answering at all is the signal. A probe that failed while a
// database was briefly away would have an orchestrator restart every
// replica of every service at once, over an outage none of them can fix.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// A probe hanging up mid-write is normal — drop the error.
		_, _ = w.Write(body)
	}
}
