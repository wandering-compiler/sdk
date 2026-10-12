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
// `Disabled` turns off generating, echoing and attaching the id —
// useful when the deployment fronts the gateway with a layer
// that already injects request IDs and we don't want to overwrite.
// A disabled gateway still applies the shape rule to the header it
// hands on (see [dropMalformedRequestID]): the fronting layer's
// well-formed id passes untouched, a malformed one does not.
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
// Generated REST gateways use
// [ObservabilityMiddleware] (in otel.go) instead of stacking
// this middleware separately — that combined wrap also tags
// the OTel span with the request_id attribute in one ctx
// mutation. This standalone form stays for non-OTel callers
// (custom HTTP servers that want correlated logs without the
// OTel pipeline).
func RequestIDMiddleware(cfg RequestIDConfig, next http.Handler) http.Handler {
	header := cfg.Header
	if header == "" {
		header = DefaultRequestIDHeader
	}
	if cfg.Disabled {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, dropMalformedRequestID(r, header))
		})
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
		next.ServeHTTP(w, withRequestID(r, header, id))
	})
}

// withRequestID attaches the chosen id to the request's context and makes it
// the only value of the request-id header on the request handed on.
//
// The header matters as much as the context: header propagation forwards the
// request's own header to the backend, so without the rewrite the backend
// received whatever the client sent — the unvalidated value the check above
// exists to keep out of logs, or nothing when the gateway generated the id —
// while the gateway logged and echoed another. The caller's header map is
// never modified; a copy is made only when the value has to change.
func withRequestID(r *http.Request, header, id string) *http.Request {
	r = r.WithContext(context.WithValue(r.Context(), requestIDCtxKey{}, id))
	if vals := r.Header.Values(header); len(vals) != 1 || vals[0] != id {
		h := r.Header.Clone()
		if h == nil {
			h = http.Header{}
		}
		h.Set(header, id)
		r.Header = h
	}
	return r
}

// dropMalformedRequestID is the disabled-mode half of the request-id rule.
//
// With the id disabled the gateway generates nothing and echoes nothing: the
// layer in front of it owns the id. But header propagation still forwards the
// request-id header to the backend, which logs it — so a client talking to the
// gateway directly could put an unbounded or control-character value into the
// backend's logs past the very check the enabled mode exists for. The header
// is therefore handed on only when it is a single value of the accepted shape
// ([validRequestID]), which every id a fronting layer generates is; anything
// else is removed, not replaced. The caller's header map is never modified.
func dropMalformedRequestID(r *http.Request, header string) *http.Request {
	vals := r.Header.Values(header)
	if len(vals) == 0 || (len(vals) == 1 && validRequestID(vals[0])) {
		return r
	}
	h := r.Header.Clone()
	h.Del(header)
	r = r.WithContext(r.Context())
	r.Header = h
	return r
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
