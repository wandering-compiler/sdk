// REST → gRPC outgoing-metadata propagation (REV-020). The
// generated gateway wraps its mux with this middleware when the
// REST registry declares `metadata_propagation`. For every
// configured header the middleware reads the incoming request
// (via `r.Header.Get`) and, when present + non-empty, appends
// the value to the outgoing gRPC metadata under the lowercase
// header name (gRPC metadata keys are case-insensitive but
// conventionally lowercase).
//
// Scoped at the api level — replaces the per-method
// `(w17.http_required_headers) → AppendToOutgoingContext` emit
// the gateway used pre-REV-020. Required-ness, format checks,
// and per-method consumption of header values now live on
// `(w17.field)` annotations + explicit `fields[]` HEADER
// bindings (handler-side decode into the request msg).
//
// Defaults match the W3C trace-context + correlation header set
// every observability stack expects (`X-Request-Id`,
// `traceparent`, `tracestate`, `baggage`); the parser resolves
// the final list before reaching the generator, so this
// middleware sees the literal header set to forward.

package restgw

import (
	"net/http"
	"strings"

	"google.golang.org/grpc/metadata"
)

// MetadataPropagationMiddleware forwards a configured set of
// HTTP headers to the outgoing gRPC metadata for every request
// reaching `next`. When `headers` is empty the wrap is a
// pass-through — operators who don't need propagation pay
// nothing.
//
// Header values reach the upstream gRPC handler via
// `metadata.FromIncomingContext`. Key normalisation:
// metadata key = `strings.ToLower(headerName)` (precomputed,
// no per-request allocation).
//
// Headers absent / blank on the incoming request are skipped —
// the middleware never injects empty metadata keys.
func MetadataPropagationMiddleware(headers []string, next http.Handler) http.Handler {
	if len(headers) == 0 {
		return next
	}
	keys := make([]string, len(headers))
	for i, h := range headers {
		keys[i] = strings.ToLower(h)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var pairs []string
		for i, h := range headers {
			v := r.Header.Get(h)
			if v == "" {
				continue
			}
			pairs = append(pairs, keys[i], v)
		}
		if len(pairs) > 0 {
			ctx := metadata.AppendToOutgoingContext(r.Context(), pairs...)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}
