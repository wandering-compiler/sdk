// Package secheaders sets the security response headers every listener a
// generated binary publishes sends: the REST gateway (its API, /healthz and
// the OAuth server), the admin, and the MCP endpoint.
//
// They are the application's, not only the edge's, because not every target
// has an edge that can add them: an AWS ALB adds no response headers at all.
// Where an edge does add them (the swarm target's Caddy and nginx), it adds
// only what the application did not send, so a handler's stricter value —
// the OAuth code page's `Referrer-Policy: no-referrer` — survives the edge.
//
// What each one does:
//
//   - Strict-Transport-Security: the browser reaches this host, and every
//     host below it, over https only, for a year. Sent over http too: a
//     browser ignores it there (RFC 6797 §8.1), so a local http run is
//     unaffected. No `preload` — that one is a submission to the browsers'
//     built-in lists and is not taken back quickly.
//   - X-Content-Type-Options: nosniff — a response is what its Content-Type
//     says, never sniffed into script or HTML.
//   - Content-Security-Policy: frame-ancestors 'none', and X-Frame-Options:
//     DENY for browsers that predate it — no page of another site frames
//     these responses (clickjacking). Only frame-ancestors: a script or style
//     policy depends on the page, and a handler that serves HTML sets its own.
//   - Referrer-Policy: strict-origin-when-cross-origin — another site learns
//     at most this origin, never a path or query.
package secheaders

import "net/http"

// Defaults are the headers [Middleware] sets, by name.
var Defaults = map[string]string{
	"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
	"X-Content-Type-Options":    "nosniff",
	"Content-Security-Policy":   "frame-ancestors 'none'",
	"X-Frame-Options":           "DENY",
	"Referrer-Policy":           "strict-origin-when-cross-origin",
}

// Middleware sets [Defaults] on every response before next runs, so a
// handler that sets one of them itself (Header().Set) replaces it, and every
// early return — a CORS preflight, a 404, a recovered panic — still carries
// them. Wrap it outermost.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		for k, v := range Defaults {
			h.Set(k, v)
		}
		next.ServeHTTP(w, r)
	})
}
