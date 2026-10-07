package restgw

import (
	"net/http"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// AdmitPublic gates a unary REST method that takes no credential
// (exclude_auth) on the surface's per-client limiter. It answers 429 itself
// and returns ok=false when the caller's bucket is drained; otherwise it
// returns the writer the handler must write through and a done func to
// defer, which charges the caller when the response was a failure (>= 400).
//
// Only failures are charged, for the reason [ratelimit.Limiter.AllowChargeable]
// gives: the limit bounds guesses, and a legitimate client making many right
// calls (an office behind one NAT address, a script signing in per request)
// is not what it defends against. A nil limiter admits everything.
func AdmitPublic(l *ratelimit.Limiter, w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func(), bool) {
	allowed, charge := l.AllowChargeableRequest(r)
	if !allowed {
		WriteTooManyRequests(w)
		return w, func() {}, false
	}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	return rec, func() {
		if rec.status >= http.StatusBadRequest {
			charge()
		}
	}, true
}

// AdmitPublicStream is AdmitPublic for a streaming method (SSE, WebSocket):
// every opened stream is charged, because its outcome is not a status, and
// the writer is left unwrapped so flushing and upgrades reach the server's
// own. A nil limiter admits everything.
func AdmitPublicStream(l *ratelimit.Limiter, w http.ResponseWriter, r *http.Request) bool {
	if !l.AllowRequest(r) {
		WriteTooManyRequests(w)
		return false
	}
	return true
}

// WriteTooManyRequests answers a refused caller: 429, RESOURCE_EXHAUSTED and
// a Retry-After the client can honour.
func WriteTooManyRequests(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	WriteError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "too many requests; try again shortly")
}

// statusRecorder remembers the status a handler wrote. Unwrap lets
// http.ResponseController reach the server's writer (deadlines, flushing).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
