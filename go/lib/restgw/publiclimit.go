package restgw

import (
	"context"
	"net/http"

	"google.golang.org/grpc/codes"

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
)

// AdmitPublic gates a unary REST method that takes no credential
// (exclude_auth) on the surface's per-client limiter. It answers 429 itself
// and returns ok=false when the caller's bucket is drained; otherwise it
// returns the writer the handler must write through and a done func to
// defer.
//
// The caller's token is taken at admission and done gives it back when the
// response was a success (< 400), so in the end only failures are charged,
// for the reason [ratelimit.Limiter.Reserve] gives: the limit bounds guesses,
// and a legitimate client making many right calls (an office behind one NAT
// address, a script signing in per request) is not what it defends against.
// Taking the token before the handler runs is what bounds requests that are
// in flight together: each holds a token, so concurrency cannot multiply the
// budget. A handler that wrote nothing at all (it panicked, say) is charged.
// A nil limiter admits everything.
func AdmitPublic(l *ratelimit.Limiter, w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func(), bool) {
	allowed, settle := l.ReserveRequest(r)
	if !allowed {
		WriteTooManyRequestsCtx(r.Context(), w)
		return w, func() {}, false
	}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	return rec, func() {
		settle(!rec.wroteHeader || rec.status >= http.StatusBadRequest)
	}, true
}

// AdmitPublicStream is AdmitPublic for a streaming method (SSE, WebSocket):
// every opened stream is charged, because its outcome is not a status, and
// the writer is left unwrapped so flushing and upgrades reach the server's
// own. A nil limiter admits everything.
//
// With [ratelimit.Config.MaxStreams] set, a client already holding that many
// open streams is refused too, without spending a token; an admitted stream
// stops counting when its request ends (the handler returned), so the call
// site needs nothing more.
func AdmitPublicStream(l *ratelimit.Limiter, w http.ResponseWriter, r *http.Request) bool {
	open, release := l.OpenStream(r)
	if !open {
		WriteTooManyRequestsCtx(r.Context(), w)
		return false
	}
	if !l.AllowRequest(r) {
		release()
		WriteTooManyRequestsCtx(r.Context(), w)
		return false
	}
	context.AfterFunc(r.Context(), release)
	return true
}

// WriteTooManyRequests answers a refused caller: 429, RESOURCE_EXHAUSTED and
// a Retry-After the client can honour. Without a request in scope the
// sentence stays in the source locale; prefer [WriteTooManyRequestsCtx].
func WriteTooManyRequests(w http.ResponseWriter) {
	WriteTooManyRequestsCtx(context.Background(), w)
}

// WriteTooManyRequestsCtx is WriteTooManyRequests with the request's context,
// so the sentence is the catalog's, in the caller's language.
//
// Nothing is reported: a refused request carries no operator detail (the
// limiter warns about the deployment conditions worth a look), and a report
// per refusal would turn a flood into a flood of reports.
func WriteTooManyRequestsCtx(ctx context.Context, w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	WriteError(w, http.StatusTooManyRequests, GRPCCodeName(codes.ResourceExhausted),
		ClientSentence(ctx, codes.ResourceExhausted))
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
