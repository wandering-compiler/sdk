// Periodic stream re-authentication.
//
// A REST request authenticates on every call. A STREAM authenticates once,
// at upgrade, and then never again — before this file there was no re-auth
// anywhere in restgw, only per-frame write deadlines. So a credential
// revoked at 10:00 kept feeding an open stream until the connection
// happened to drop, which for a healthy connection can be days.
//
// That hole predates capability links: a Bearer-authenticated stream had
// it too. It stayed survivable because a session client reconnects
// constantly — token refresh, tab reload, a network blip — and each
// reconnect re-authenticates, so revocation landed within minutes in
// practice. A capability link (a URL-carried `credential`) has no session
// lifecycle at all. Nothing ever forces it to reconnect. "In practice it
// closes itself" stops being true, which is why URL-carried credentials on
// streams had to wait for this rather than ship before it.
//
// The watchdog therefore covers EVERY authenticated stream, not only the
// URL-credential ones. Scoping it to the new feature would have left the
// older hole open while claiming the surface was fixed.
//
// # What it does NOT do
//
// It re-runs the surface's own auth function against the ORIGINAL request.
// It does not know what "revoked" means — that is the auth backend's
// judgement, exactly as it is on a unary call. A backend that keeps
// answering OK for a revoked token will keep the stream open, and that is
// the correct division: the gateway asks, the backend decides.
//
// It also cannot beat the auth CACHE. With `<PREFIX>_AUTH_CACHE_TTL_SECONDS`
// set, a re-auth inside the TTL is served from cache and answers with the
// stale verdict, so effective revocation latency is
// `max(reauth interval, cache TTL)`. Both default to values that make this
// a non-issue (cache off, interval 5m), but an operator who raises the
// cache TTL has raised this too.
package restgw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// DefaultStreamReauthInterval bounds how long a revoked credential can keep
// an already-open stream alive.
//
// Five minutes is chosen against the cost, not against a threat model: the
// watchdog calls the auth backend once per interval per OPEN stream, so the
// load is (open streams / interval). At 5m a service holding 10k streams
// pays ~33 auth calls/second, which is the same order as its ordinary
// request traffic. Dropping to 30s would multiply that by ten to shave
// four and a half minutes off a rare event.
//
// Operators who need tighter revocation lower it; operators running streams
// with no revocation story at all can set 0. Both are one env var.
const DefaultStreamReauthInterval = 5 * time.Minute

// StreamReauthIntervalFromEnv reads `<PREFIX>_STREAM_REAUTH_SECONDS`.
//
// Unset → [DefaultStreamReauthInterval]. This default is ON deliberately.
// A security fix that ships switched off is a security fix nobody gets:
// the deployments that most need it are exactly the ones not reading
// release notes for env vars to add.
//
// `0` (or negative) disables the watchdog — an explicit opt-out an operator
// has to type, which is the difference between a considered decision and a
// default nobody chose.
//
// An unparseable value also falls back to the default rather than to off.
// A typo in a security knob must not silently disable it.
func StreamReauthIntervalFromEnv(prefix string, getenv func(string) string) time.Duration {
	if getenv == nil {
		return DefaultStreamReauthInterval
	}
	raw := getenv(prefix + "_STREAM_REAUTH_SECONDS")
	if raw == "" {
		return DefaultStreamReauthInterval
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultStreamReauthInterval
	}
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// WatchStreamAuth returns a context that is cancelled as soon as re-running
// authFn against r is REFUSED, plus a stop function the caller MUST defer.
//
// Cancelling the context is the whole mechanism: every generated streaming
// handler already threads its rpc context into the backend call and its
// send loop, so a cancel tears the stream down through the paths that
// already exist rather than through a second shutdown mechanism that could
// disagree with the first.
//
// # Refused versus unanswered
//
// Only a definite refusal ends the stream: a gRPC status of
// UNAUTHENTICATED or PERMISSION_DENIED from the auth method, or the ticket
// sentinels [ErrWSAuthNoTicket] / [ErrTicketInvalid]. Everything else — a
// probe that timed out, UNAVAILABLE from a backend that is down, any other
// status, a non-status error from the dial — means the auth backend never
// rendered a verdict. Reading that as "revoked" would let one auth outage
// disconnect every live stream on the surface within one interval, turning
// a partial failure into a total one. An unanswered probe is reported to
// the operator instead and asked again sooner than the interval (backing
// off from [streamReauthRetryMin] up to the interval itself), so a
// revocation still lands at the first answer after the backend recovers.
// This is the same split [RequireAuth] makes on the way in, where a
// non-status error is a 500 and not a 401.
//
// # The principal, not just the credential
//
// A successful probe can still change WHO the credential stands for — a
// membership removed, a role narrowed. The latest verdict is kept on the
// returned context, and a consumer whose entitlement was decided at connect
// time (the `w17-events` hub, see [EventbusEventSource]) re-derives it
// after every probe and ends its stream when it changed.
//
// interval <= 0 disables the watchdog and returns ctx unchanged with a
// no-op stop, so a caller never has to branch.
//
// authFn nil is treated the same way. A surface with no auth methods wires
// [NoAuth], which always succeeds — polling it would burn a goroutine per
// stream to learn nothing.
func WatchStreamAuth(ctx context.Context, authFn AuthFunc, r *http.Request, interval time.Duration) (context.Context, context.CancelFunc) {
	if interval <= 0 || authFn == nil || r == nil {
		return ctx, func() {}
	}
	return watchStream(ctx, authFn, r, interval, isAuthRefusal)
}

// watchStream is the watchdog loop behind [WatchStreamAuth] and
// [WatchStreamBinding]: probe is asked every interval, refused decides which
// errors are a verdict. The caller has checked its arguments.
func watchStream(ctx context.Context, authFn AuthFunc, r *http.Request, interval time.Duration, refused func(error) bool) (context.Context, context.CancelFunc) {
	watched, cancel := context.WithCancel(ctx)
	principal := &streamPrincipal{
		userData: AuthUserDataFromContext(r.Context()),
		changed:  make(chan struct{}, 1),
	}
	watched = context.WithValue(watched, streamPrincipalKey{}, principal)
	// The request carries the credential (a header, or the reserved slot the
	// gateway filled from the URL). Detach it from the ORIGINAL request's
	// context before re-using it: that context ends when the stream ends,
	// and an auth call inherited from it would be cancelled by the very
	// shutdown it is supposed to be independent of.
	//
	// Also detach from `watched`, not just from r.Context(): once the
	// watchdog cancels, an in-flight auth call carrying that context would
	// fail with "context canceled" and be indistinguishable from a genuine
	// auth failure in the logs.
	probe := r.Clone(context.WithoutCancel(ctx))
	go func() {
		timer := time.NewTimer(interval)
		defer timer.Stop()
		// Consecutive unanswered probes; drives the retry backoff.
		unanswered := 0
		for {
			select {
			case <-watched.Done():
				return
			case <-timer.C:
				// Bound each probe so a hung auth backend cannot pin this
				// goroutine for the lifetime of the process.
				probeCtx, probeCancel := context.WithTimeout(probe.Context(), streamReauthProbeTimeout)
				userData, err := authFn(probeCtx, probe.WithContext(probeCtx))
				probeCancel()
				switch {
				case err == nil:
					unanswered = 0
					principal.publish(userData)
					timer.Reset(interval)
				case refused(err):
					cancel()
					return
				default:
					unanswered++
					reportUnansweredReauth(probe.Context(), err)
					timer.Reset(streamReauthRetryDelay(unanswered, interval))
				}
			}
		}
	}()
	return watched, cancel
}

// isAuthRefusal reports whether an auth error is the backend's verdict that
// the credential is no good — as opposed to the backend not answering.
//
// The two codes are the ones [WriteAuthError] turns into 401 and 403. The
// ticket sentinels are what [NewWSAuth] returns for a request whose only
// credential is a ticket that cannot be redeemed; a re-auth probe normally
// carries the promoted Authorization instead, so reaching them means the
// stream has no credential left to show.
func isAuthRefusal(err error) bool {
	if errors.Is(err, ErrWSAuthNoTicket) || errors.Is(err, ErrTicketInvalid) {
		return true
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied:
		return true
	}
	return false
}

// defaultStreamReauthRetryMin is the first retry delay after an unanswered
// probe; each further unanswered probe doubles it, up to the interval.
const defaultStreamReauthRetryMin = 5 * time.Second

// streamReauthRetryMin overrides the first retry delay when positive.
// Atomic so a test can shorten it while watchdogs of other tests are still
// running.
var streamReauthRetryMin atomic.Int64

// streamReauthRetryDelay is the wait before the next probe after the n-th
// consecutive unanswered one. It never exceeds the interval — an outage
// must not make revocation SLOWER than it is on a healthy backend — and it
// never drops below streamReauthRetryMin, so an auth backend that is down
// is not hammered by every open stream at once.
func streamReauthRetryDelay(n int, interval time.Duration) time.Duration {
	d := defaultStreamReauthRetryMin
	if o := time.Duration(streamReauthRetryMin.Load()); o > 0 {
		d = o
	}
	for i := 1; i < n && d < interval; i++ {
		d *= 2
	}
	if d > interval {
		d = interval
	}
	return d
}

// reauthReportEvery throttles the operator report for unanswered probes.
// One auth outage makes every open stream's probe fail at about the same
// moment; one report per window says the same thing as ten thousand.
const reauthReportEvery = time.Minute

// reauthLastReport is the unix-nano time of the last unanswered-probe
// report, shared by every watchdog in the process.
var reauthLastReport atomic.Int64

// reportUnansweredReauth tells the operator that a re-auth probe got no
// verdict. It is a fault — the auth backend is unreachable or broken — so it
// goes on the exception channel, throttled process-wide.
func reportUnansweredReauth(ctx context.Context, err error) {
	now := time.Now().UnixNano()
	last := reauthLastReport.Load()
	if last != 0 && now-last < int64(reauthReportEvery) {
		return
	}
	if !reauthLastReport.CompareAndSwap(last, now) {
		return
	}
	observx.ReportError(ctx, fmt.Errorf("stream re-auth: auth backend gave no verdict; streams stay open and are re-probed: %w", err))
}

// streamReauthProbeTimeout bounds one re-auth call. Generous relative to a
// healthy auth backend and short relative to the interval, so a slow probe
// cannot still be running when the next tick fires.
const streamReauthProbeTimeout = 10 * time.Second

// streamPrincipal is the latest successful verdict of a stream's re-auth
// watchdog, published on the watched context so a consumer that derived
// something from the principal at connect time can notice it changed.
type streamPrincipal struct {
	mu       sync.Mutex
	userData []byte
	// changed carries one pending notification (capacity 1): a consumer
	// only needs to know "look again", not how many times.
	changed chan struct{}
}

type streamPrincipalKey struct{}

// publish records a verdict and notifies a consumer when it differs from
// the previous one.
func (p *streamPrincipal) publish(userData []byte) {
	p.mu.Lock()
	if bytes.Equal(p.userData, userData) {
		p.mu.Unlock()
		return
	}
	p.userData = userData
	p.mu.Unlock()
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

// latest returns the most recent verdict's AuthResp bytes.
func (p *streamPrincipal) latest() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.userData
}

// streamPrincipalFrom returns the watchdog's principal record carried by
// ctx, or nil when the stream is not watched.
func streamPrincipalFrom(ctx context.Context) *streamPrincipal {
	p, _ := ctx.Value(streamPrincipalKey{}).(*streamPrincipal)
	return p
}
