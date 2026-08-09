// Periodic stream re-authentication (REV-163).
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
// practice. A capability link (REV-162 `credential`) has no session
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
	"context"
	"net/http"
	"strconv"
	"time"
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
// authFn against r stops succeeding, plus a stop function the caller MUST
// defer.
//
// Cancelling the context is the whole mechanism: every generated streaming
// handler already threads its rpc context into the backend call and its
// send loop, so a cancel tears the stream down through the paths that
// already exist rather than through a second shutdown mechanism that could
// disagree with the first.
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
	watched, cancel := context.WithCancel(ctx)
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
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-watched.Done():
				return
			case <-ticker.C:
				// Bound each probe so a hung auth backend cannot pin this
				// goroutine for the lifetime of the process. A timeout is
				// NOT treated as a failed auth — see below.
				probeCtx, probeCancel := context.WithTimeout(probe.Context(), streamReauthProbeTimeout)
				_, err := authFn(probeCtx, probe.WithContext(probeCtx))
				// Read the probe's own deadline state BEFORE cancelling it.
				// probeCancel() sets probeCtx.Err() unconditionally, so
				// checking it afterwards reports "timed out" for every
				// outcome — including a genuine rejection. The watchdog
				// then never fires, silently, which is the failure mode a
				// security control can least afford.
				timedOut := probeCtx.Err() != nil
				probeCancel()
				if err == nil {
					continue
				}
				if timedOut {
					// The probe itself timed out or was cancelled. The auth
					// backend never rendered a verdict, so treating this as
					// "revoked" would let a backend outage disconnect every
					// live stream on the surface at once — turning a partial
					// failure into a total one. Skip this round; the next
					// tick asks again.
					continue
				}
				cancel()
				return
			}
		}
	}()
	return watched, cancel
}

// streamReauthProbeTimeout bounds one re-auth call. Generous relative to a
// healthy auth backend and short relative to the interval, so a slow probe
// cannot still be running when the next tick fires.
const streamReauthProbeTimeout = 10 * time.Second
