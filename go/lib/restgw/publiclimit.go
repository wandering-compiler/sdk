package restgw

import (
	"context"
	"net/http"
	"strings"

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
// A DPoP nonce challenge is not ([NonceChallenge]). A nil limiter admits
// everything.
func AdmitPublic(l *ratelimit.Limiter, w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func(), bool) {
	allowed, settle := l.ReserveRequest(r)
	if !allowed {
		WriteTooManyRequestsCtx(r.Context(), w)
		return w, func() {}, false
	}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	return rec, func() {
		settle(!rec.wroteHeader || (rec.status >= http.StatusBadRequest && !NonceChallenge(rec.status, rec.Header())))
	}, true
}

// NonceChallenge reports whether a response is a DPoP nonce challenge: 401
// with `WWW-Authenticate: DPoP error="use_dpop_nonce"`. A client with a fresh
// key is sent one on its first request and repeats the request with the
// nonce; nothing was checked, so it is not a failed attempt, and a limit that
// charges only failures must not count it — or every first sign-in of a new
// device would spend a token of the address's guess budget.
//
// It reads the header itself rather than through lib/dpop, which stays out of
// this package so a project that never binds devices takes no DPoP
// dependency; nonce_challenge_test.go holds it to dpop.IsUseNonceChallenge.
func NonceChallenge(status int, h http.Header) bool {
	if status != http.StatusUnauthorized {
		return false
	}
	for _, v := range h.Values("WWW-Authenticate") {
		l := strings.ToLower(v)
		i := strings.Index(l, "dpop ")
		if i < 0 || (i > 0 && !strings.HasSuffix(strings.TrimRight(l[:i], " "), ",")) {
			continue
		}
		rest := strings.ReplaceAll(l[i:], " ", "")
		if strings.Contains(rest, `error="use_dpop_nonce"`) || strings.Contains(rest, "error=use_dpop_nonce") {
			return true
		}
	}
	return false
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

// AdmitPublicStreamOf is AdmitPublicStream for a surface whose public methods
// each have their own limiter ([ratelimit.Methods]): streams caps the streams
// one client holds open across the transport, and l — the method's own
// limiter, nil when the method is not limited — is charged one token per
// opened stream. Both nil admit everything.
func AdmitPublicStreamOf(streams, l *ratelimit.Limiter, w http.ResponseWriter, r *http.Request) bool {
	open, release := streams.OpenStream(r)
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
