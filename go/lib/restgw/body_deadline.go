package restgw

import (
	"mime"
	"net/http"
	"strings"
	"time"
)

// DefaultBodyReadTimeout is the bound [BodyReadDeadline] puts on reading a
// request body in the generated gateways. A body the gateway decodes whole
// is capped at MaxRequestBodyBytes, so a minute is a generous budget on any
// real link.
const DefaultBodyReadTimeout = 60 * time.Second

// BodyReadDeadline returns a middleware that bounds how long reading a
// request's BODY may take: the connection's read deadline is set to now+d
// before the handler runs, so a client trickling its body gets a read error
// at d instead of holding a goroutine and a connection for as long as it
// likes. ReadHeaderTimeout bounds only the header phase, and the decode caps
// bytes, not time.
//
// It applies to a request that carries a body the gateway reads whole. A
// request without one — an SSE GET, a WebSocket upgrade, whose connection
// then lives for as long as the stream does — is untouched, and so is a
// multipart upload, whose size is the author's declared cap and whose time
// is the uploader's bandwidth. d <= 0 disables it.
func BodyReadDeadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hasWholeBody(r) {
				// Best effort: a writer that cannot set a deadline (a
				// test recorder, an HTTP/2 stream on an old server)
				// keeps the previous behaviour.
				_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// hasWholeBody reports a request with a body the gateway reads in one go.
func hasWholeBody(r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return false
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mt, "multipart/") {
		return false
	}
	return true
}
