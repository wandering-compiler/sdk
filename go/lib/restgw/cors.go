package restgw

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// CORSConfig drives the [CORSMiddleware] wrapper. The
// generated gateway main.go reads ENV variables, populates a
// CORSConfig, and wraps its mux only when AllowedOrigins is
// non-empty. CORS is opt-in: a gateway with no origin
// allowlist behaves exactly as before (no preflight handling,
// no ACAO headers) so non-browser callers keep working
// unchanged.
//
// Why ENV-driven (not proto-annotated): origins typically vary
// per-environment (staging vs. prod vs. local dev) — burning
// them into the binary at codegen time would force one bundle
// per environment. ENV plumbing keeps a single binary
// portable across all of them.
type CORSConfig struct {
	// AllowedOrigins is the list of accepted Origin values.
	// `"*"` is the wildcard — when [AllowCredentials] is
	// false, the middleware emits a literal `*` in the
	// `Access-Control-Allow-Origin` header.
	//
	// `"*"` combined with [AllowCredentials] is FORBIDDEN open
	// credentialed CORS (the Fetch spec disallows `*` together
	// with `Access-Control-Allow-Credentials: true`, and
	// reflecting an arbitrary Origin to dodge that ban is the
	// exact hole the ban exists to close). [CORSMiddleware]
	// refuses the combination: it drops credentials (keeping the
	// open `*` but without cookies/Authorization) and logs a
	// warning. To use credentials, list the concrete origins
	// explicitly instead of `*`.
	AllowedOrigins []string

	// AllowedMethods is the methods advertised in preflight
	// responses. Empty defaults to `GET, POST, PUT, PATCH,
	// DELETE, OPTIONS` — covers every verb the gateway
	// currently emits.
	AllowedMethods []string

	// AllowedHeaders is the request headers advertised in
	// preflight responses. Empty defaults to `Content-Type,
	// Authorization` — the two headers every browser
	// frontend sends. Operators add domain-specific entries
	// (`X-Tenant-Id`, etc.) via the `<ENV>_CORS_HEADERS` env.
	AllowedHeaders []string

	// AllowCredentials enables the `Access-Control-Allow-
	// Credentials: true` header. Browsers refuse to send
	// cookies / Authorization on cross-origin requests
	// without it, so consumers using session-cookie auth
	// must turn this on.
	AllowCredentials bool

	// MaxAgeSeconds caches the preflight response on the
	// browser side. 0 disables the `Access-Control-Max-Age`
	// header (browsers fall back to their own default —
	// typically 5 seconds, which spams preflight). 600 is a
	// sane default; consumers tune via env.
	MaxAgeSeconds int
}

// CORSMiddleware returns an http.Handler that wraps `next`
// with CORS handling. Disabled when `cfg.AllowedOrigins` is
// empty — the function returns `next` unchanged, so the
// generator can call this unconditionally and pay no cost
// when CORS is off.
//
// Behavior on every request:
//
//   - If the request carries an `Origin` header matching the
//     allowlist, the response gains `Access-Control-Allow-
//     Origin` (echo-back or `*`), `Vary: Origin`, and
//     `Access-Control-Allow-Credentials` when configured.
//   - If the method is OPTIONS AND
//     `Access-Control-Request-Method` is set, this is a
//     preflight: the response carries `Allow-Methods` /
//     `Allow-Headers` / optional `Max-Age` and 204s without
//     dispatching to `next`.
//   - Otherwise `next` runs and sees the augmented response
//     headers (already written above).
//
// Mismatched origins still flow through to `next` — the spec
// puts enforcement on the browser. Generated handlers don't
// see CORS at all; the wrapper layers in main.go.
func CORSMiddleware(cfg CORSConfig, next http.Handler) http.Handler {
	if len(cfg.AllowedOrigins) == 0 {
		return next
	}
	// Open credentialed CORS is forbidden by the Fetch spec: a `*`
	// origin allowlist reflects an arbitrary request Origin, and
	// pairing that reflection with `Access-Control-Allow-Credentials:
	// true` would let ANY website make credentialed cross-origin calls
	// against this gateway (read responses authenticated by the user's
	// cookies / Authorization). Refuse the combination by dropping
	// credentials — the safe direction: the wildcard keeps working, it
	// just stops carrying credentials — and warn loudly so the operator
	// narrows the allowlist to the concrete origins that actually need
	// cookies. A credentialed setup requires an explicit origin list,
	// never `*`. (R-restgw-2)
	allowCredentials := cfg.AllowCredentials
	if allowCredentials && containsWildcardOrigin(cfg.AllowedOrigins) {
		slog.Warn("restgw: CORS credentials disabled — a wildcard '*' origin allowlist cannot be combined with Access-Control-Allow-Credentials (forbidden open credentialed CORS); set an explicit origin list to keep credentials")
		allowCredentials = false
	}
	methods := cfg.AllowedMethods
	if len(methods) == 0 {
		methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	}
	headers := cfg.AllowedHeaders
	if len(headers) == 0 {
		headers = []string{"Content-Type", "Authorization"}
	}
	methodsHdr := strings.Join(methods, ", ")
	headersHdr := strings.Join(headers, ", ")
	maxAgeHdr := ""
	if cfg.MaxAgeSeconds > 0 {
		maxAgeHdr = strconv.Itoa(cfg.MaxAgeSeconds)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allow := matchOrigin(origin, cfg.AllowedOrigins, allowCredentials); allow != "" {
			w.Header().Set("Access-Control-Allow-Origin", allow)
			w.Header().Add("Vary", "Origin")
			if allowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", methodsHdr)
			w.Header().Set("Access-Control-Allow-Headers", headersHdr)
			if maxAgeHdr != "" {
				w.Header().Set("Access-Control-Max-Age", maxAgeHdr)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// containsWildcardOrigin reports whether the allowlist contains the
// bare `*` wildcard entry. Used to refuse the forbidden `*` +
// credentials combination at middleware construction (R-restgw-2).
func containsWildcardOrigin(allowed []string) bool {
	for _, a := range allowed {
		if a == "*" {
			return true
		}
	}
	return false
}

// matchOrigin returns the Allow-Origin value to write back
// for a given (origin, allowlist, credentials) triple — empty
// string when the origin doesn't pass.
//
// Wildcard rules:
//   - `"*"` in the allowlist + credentials disabled → bare
//     `"*"` echo (every browser accepts; no cookie carrying).
//   - Exact match → echo (this is how credentialed CORS works:
//     a concrete origin is named, so `Allow-Credentials: true`
//     is legal alongside it).
//   - No match → empty string (no ACAO header).
//
// The `allowCredentials` argument is the EFFECTIVE flag, which
// [CORSMiddleware] forces to false whenever the allowlist holds
// `"*"`. So the `"*"` + credentials path never reaches here from
// the gateway. The defensive branch below still reflects the
// concrete origin if a direct caller passes that forbidden combo,
// but the middleware is the policy layer that prevents it.
func matchOrigin(origin string, allowed []string, allowCredentials bool) string {
	if origin == "" {
		return ""
	}
	for _, a := range allowed {
		if a == "*" {
			if allowCredentials {
				return origin
			}
			return "*"
		}
		if a == origin {
			return origin
		}
	}
	return ""
}

// CORSConfigFromEnv builds a CORSConfig from the ENV-var
// names the generated gateway main.go uses. Callers pass the
// per-bundle ENV prefix; this helper composes the canonical
// suffixes (`_CORS_ORIGINS`, etc.) and parses CSV values.
//
// Returns the zero value when `<prefix>_CORS_ORIGINS` is
// unset — the middleware will pass through unchanged.
//
// Lookup is supplied by the caller (typically `os.Getenv`)
// so tests can drive the helper without process-global state.
func CORSConfigFromEnv(prefix string, lookup func(string) string) CORSConfig {
	origins := splitCSV(lookup(prefix + "_CORS_ORIGINS"))
	if len(origins) == 0 {
		return CORSConfig{}
	}
	cfg := CORSConfig{
		AllowedOrigins:   origins,
		AllowedMethods:   splitCSV(lookup(prefix + "_CORS_METHODS")),
		AllowedHeaders:   splitCSV(lookup(prefix + "_CORS_HEADERS")),
		AllowCredentials: parseBool(lookup(prefix + "_CORS_ALLOW_CREDENTIALS")),
		// Default 600s (10 min) — production-idiomatic per
		// MDN + Mozilla best-practice (REV-032 Cat 4 sweep,
		// F13). Without an explicit Access-Control-Max-Age
		// header browsers fall back to ~5s, which spams
		// preflight on every API call from SPAs. Operators
		// who want the browser default back set
		// `<PREFIX>_CORS_MAX_AGE=0` explicitly.
		MaxAgeSeconds: 600,
	}
	if v := lookup(prefix + "_CORS_MAX_AGE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.MaxAgeSeconds = n
		}
	}
	return cfg
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
