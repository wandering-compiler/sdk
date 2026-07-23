// HTTP-side OpenTelemetry + Prometheus surface for generated
// REST gateways (G3i3-GW-C + G3i3-GW-D, REV-031 Phase C-6).
// OTel boot itself (TracerProvider + MeterProvider + W3C
// propagator) lives in `lib/observx` — this file only carries
// the HTTP-specific bits:
//
//   - **ObservabilityMiddleware**: combines otelhttp wrap +
//     RequestID generation/echo + request_id-as-span-attribute
//     (REV-032 Cat 4 sweep). One wrap, one ctx mutation, span
//     attribute set in the same hop — generated gateway uses
//     this instead of two separate middlewares.
//   - **OTelMiddleware**: standalone otelhttp wrap. Kept for
//     non-gateway callers (custom HTTP servers that want OTel
//     but not the request-ID convention).
//   - **MetricsHandler / MountMetricsListener**: exposes the
//     Prometheus collectors observx populated when
//     `EnableMetrics: true` was passed to MustInit.
//
// ENV surface (gateway main.go):
//
//	W17_OTEL_ENDPOINT=...             — observx wires OTLP/gRPC exporter
//	W17_SENTRY_DSN=...                — observx wires Sentry
//	<PREFIX>_METRICS_PORT=9090        — restgw exposes /metrics on
//	                                    a separate listener (kept off
//	                                    the public mux to keep
//	                                    per-route labels operator-only)

package restgw

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	// otelgrpc is referenced from the generated gateway
	// `main.go` template (gRPC backend client tracing
	// hookup); blank-importing it here pins the dep as a
	// direct require so `go mod tidy` doesn't drop it from
	// the compiler's go.mod. The version flows through DepVersions
	// into each emitted gateway bundle's go.mod (per
	// `feedback_verify_tool_versions` — no hardcoded values).
	_ "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ObservabilityMiddleware combines OTel HTTP instrumentation +
// request-ID handling into a single wrap (REV-032, Cat 4
// sweep). Sequence per request:
//
//  1. otelhttp.NewHandler wraps the chain — span starts on
//     entry; otelhttp emits standard `http.server.*` attrs +
//     histogram bucket.
//  2. Inner handler reads the configured request-ID header
//     (default `X-Request-ID`); generates a UUIDv4 when
//     missing; echoes back on the response under the same
//     header; attaches to ctx via [RequestIDFromContext].
//  3. Sets the request_id as a span attribute when a recording
//     span exists — one source of truth for request
//     correlation across logs (RequestID), traces (OTel), and
//     errors (observx.ReportError reads both).
//
// Why combined: avoids stacking two `http.Handler` wrappers
// for what is effectively one observability concern; the
// span attr write needs the active OTel span which only
// exists INSIDE otelhttp.NewHandler — the natural place is
// here.
//
// Disabled-RequestID path (`requestIDCfg.Disabled = true`)
// skips the header generation + ctx attach but still wraps
// otelhttp. Gateway operators turn the request-ID off when
// fronted by infra that already injects one.
func ObservabilityMiddleware(next http.Handler, serviceName string, requestIDCfg RequestIDConfig) http.Handler {
	header := requestIDCfg.Header
	if header == "" {
		header = DefaultRequestIDHeader
	}
	disabled := requestIDCfg.Disabled
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !disabled {
			id := strings.TrimSpace(r.Header.Get(header))
			// B40-restgw-1: validate the client-supplied id before echoing it
			// into the response header + correlating it into the span/logs — an
			// unbounded / special-char value enables log-forging (restgw-sec-4).
			// RequestIDMiddleware already guards this; the generated gateways
			// wire THIS middleware, so it must enforce the same. A malformed (or
			// empty) id is replaced with a fresh UUID.
			if !validRequestID(id) {
				id = uuid.NewString()
			}
			w.Header().Set(header, id)
			ctx := context.WithValue(r.Context(), requestIDCtxKey{}, id)
			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.SetAttributes(attribute.String("request_id", id))
			}
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
	return otelhttp.NewHandler(inner, serviceName)
}

// OTelMiddleware wraps every HTTP handler in an OTel span +
// records the standard `http.server.request.duration`
// histogram via otelhttp's auto-instrumentation. Span name
// defaults to the route pattern (Go 1.22+ ServeMux exposes
// it via r.Pattern); manual override possible via
// WithSpanNameFormatter on the otelhttp.NewHandler call.
//
// Always wrap — when observx didn't wire an OTel exporter
// the global TracerProvider is the SDK noop, so the wrap is
// a near-zero-cost pass-through. Operators flip OTel on/off
// via `W17_OTEL_ENDPOINT` (empty = noop) without touching
// the gateway code.
//
// Generated gateways prefer [ObservabilityMiddleware] which
// also folds in request-ID handling; this standalone form
// stays for non-gateway callers (custom HTTP servers that
// want OTel without the request-ID convention).
func OTelMiddleware(next http.Handler, serviceName string) http.Handler {
	return otelhttp.NewHandler(next, serviceName)
}

// MetricsHandler exposes the Prometheus collector observx
// registered (when MustInit ran with `EnableMetrics: true`).
// main.go mounts this on a separate listener port
// (<PREFIX>_METRICS_PORT) so the metrics endpoint doesn't
// show up on the public mux.
//
// When observx didn't enable metrics the handler returns
// whatever the global default Prometheus registry has
// (typically the Go runtime collectors only) — still a
// valid response, just without the per-request HTTP
// histograms.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

// MountMetricsListener starts a goroutine serving the
// Prometheus exporter on <PREFIX>_METRICS_PORT (when set).
// Returns the listener address ("" when the env is unset)
// so main.go can log the binding. Errors fall through log
// (operator-visible) but don't kill the main process.
//
// The metrics listener is intentionally separate from the
// public mux: metrics endpoints carry detailed per-route
// labels (method / status / route pattern) that operators
// don't want exposed to public callers.
func MountMetricsListener(prefix string, lookup func(string) string, log func(format string, args ...any)) string {
	port := lookup(prefix + "_METRICS_PORT")
	if port == "" {
		return ""
	}
	addr := ":" + port
	mux := http.NewServeMux()
	mux.Handle("/metrics", MetricsHandler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log("metrics listener on %s: %v", addr, err)
		}
	}()
	return addr
}
