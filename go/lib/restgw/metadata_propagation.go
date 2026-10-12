// REST → gRPC outgoing-metadata propagation. The
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
// the gateway used before. Required-ness, format checks,
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
	"log"
	"net/http"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/principal"
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
//
// A header whose metadata key is reserved for the gateway
// ([principal.IsEdgeReservedKey]: `x-w17-user`, `x-w17-scope-*`,
// `w17-label-*`, the transaction keys, `grpcgateway-*`) is never
// forwarded, whatever the list says, and is logged once here. Those
// keys reach the backend only from the gateway's own stamping of the
// verified auth response; forwarded from a client they would be a
// principal or tenant scope the caller chose — on a route without
// auth nothing overwrites them, and on one with auth a scope or label
// the response omits keeps the client's value, which the backend reads
// first. The generator refuses such a list too; this is the runtime
// half, for every gateway that does not come from it.
func MetadataPropagationMiddleware(headers []string, next http.Handler) http.Handler {
	headers = withoutReservedHeaders(headers)
	if len(headers) == 0 {
		return next
	}
	keys := make([]string, len(headers))
	for i, h := range headers {
		keys[i] = metadataKeyFor(h)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var pairs []string
		for i, h := range headers {
			v := headerValue(r, h)
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

// metadataKeyFor is the gRPC metadata key a forwarded header travels under:
// its lower-cased name, except for the two the gRPC hop owns. `host` is the
// transport's :authority (the backend's address) and `user-agent` is replaced
// by the client's own ("grpc-go/…"), so forwarded under their own names the
// browser's values never arrive. They travel as x-forwarded-host and
// x-forwarded-user-agent instead (found live by examples/auth-proof: every
// device was labelled grpc-go, and tenant_scope never saw a Host).
func metadataKeyFor(header string) string {
	switch k := strings.ToLower(header); k {
	case "host":
		return "x-forwarded-host"
	case "user-agent":
		return "x-forwarded-user-agent"
	default:
		return k
	}
}

// headerValue reads a header to forward. Go keeps the request's Host out of
// r.Header (it is r.Host), so Host read from the map was always empty.
func headerValue(r *http.Request, header string) string {
	if strings.EqualFold(header, "host") {
		return r.Host
	}
	return r.Header.Get(header)
}

// withoutReservedHeaders drops the propagation entries whose metadata key
// the gateway reserves, logging each one. Returns headers itself when
// nothing is dropped.
func withoutReservedHeaders(headers []string) []string {
	for i, h := range headers {
		if !principal.IsEdgeReservedKey(metadataKeyFor(h)) {
			continue
		}
		kept := append(make([]string, 0, len(headers)-1), headers[:i]...)
		for _, h := range headers[i:] {
			if principal.IsEdgeReservedKey(metadataKeyFor(h)) {
				logReservedMetadataKey("propagation header", h)
				continue
			}
			kept = append(kept, h)
		}
		return kept
	}
	return headers
}

// logReservedMetadataKey reports a configuration entry that targets a
// gateway-reserved metadata key and is therefore ignored.
func logReservedMetadataKey(what, key string) {
	log.Printf("restgw: metadata %s %q targets a gateway-reserved key and is ignored; x-w17-*, w17-label-*, the transaction keys and grpcgateway-* are set only by the gateway itself, never from a client header", what, key)
}
