// X-Request-ID propagation middleware (G3i3-GW-Misc-D). On every
// request:
//
//   - Read the configured request-ID header from the incoming
//     request (default `X-Request-ID`). Use it verbatim when
//     present + non-empty.
//   - Generate a fresh UUIDv4 when the header is missing.
//   - Echo the chosen ID back on the response under the same
//     header.
//   - Attach the ID to the request context so downstream
//     handlers can read it via `RequestIDFromContext`.
//
// Independent from OTel's W3C trace-id propagation: the request
// ID survives boundaries that strip OTel headers (logs, audit
// trails, customer-facing error pages) and is opaque to the
// observability stack. When OTel is on, downstream code can
// attach the request ID as a span attribute for correlation —
// `RequestIDFromContext` is the lookup hook.

package restgw

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// DefaultRequestIDHeader is the standard header name; matches
// the de-facto convention across reverse proxies + APM vendors
// (Cloudflare, Datadog, Heroku, Sentry, Stripe, etc.). Override
// per-deployment via `<PREFIX>_REQUEST_ID_HEADER` when integrating
// with infra that uses a different name.
const DefaultRequestIDHeader = "X-Request-ID"

// RequestIDConfig captures the resolved per-binary settings.
// `Disabled` short-circuits the middleware to a pass-through —
// useful when the deployment fronts the gateway with a layer
// that already injects request IDs and we don't want to overwrite.
type RequestIDConfig struct {
	Header   string
	Disabled bool
}

// RequestIDConfigFromEnv reads:
//
//	<PREFIX>_REQUEST_ID_HEADER          — header name override
//	<PREFIX>_REQUEST_ID_DISABLED=true   — turns the middleware off
//
// Both env vars are optional. Default header is
// [DefaultRequestIDHeader]; default state is enabled.
func RequestIDConfigFromEnv(prefix string, lookup func(string) string) RequestIDConfig {
	cfg := RequestIDConfig{Header: DefaultRequestIDHeader}
	if h := strings.TrimSpace(lookup(prefix + "_REQUEST_ID_HEADER")); h != "" {
		cfg.Header = h
	}
	if strings.EqualFold(strings.TrimSpace(lookup(prefix+"_REQUEST_ID_DISABLED")), "true") {
		cfg.Disabled = true
	}
	return cfg
}

type requestIDCtxKey struct{}

// RequestIDFromContext returns the request ID attached by
// [RequestIDMiddleware], or empty string when the middleware
// didn't run on this request.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDCtxKey{}).(string)
	return id
}

// RequestIDMiddleware threads a request ID through every
// request reaching `next`. See package doc.
//
// REV-032 Cat 4 sweep: generated REST gateways use
// [ObservabilityMiddleware] (in otel.go) instead of stacking
// this middleware separately — that combined wrap also tags
// the OTel span with the request_id attribute in one ctx
// mutation. This standalone form stays for non-OTel callers
// (custom HTTP servers that want correlated logs without the
// OTel pipeline).
func RequestIDMiddleware(cfg RequestIDConfig, next http.Handler) http.Handler {
	if cfg.Disabled {
		return next
	}
	header := cfg.Header
	if header == "" {
		header = DefaultRequestIDHeader
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(header))
		// Validate the client-supplied id before echoing it into the
		// response header + correlating it into logs: an unbounded /
		// special-char value enables log-forging (restgw-sec-4). A
		// malformed id is replaced with a fresh UUID.
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		w.Header().Set(header, id)
		ctx := context.WithValue(r.Context(), requestIDCtxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validRequestID accepts a non-empty, length-bounded id made only of
// characters safe to echo into a header and a log line.
func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}
