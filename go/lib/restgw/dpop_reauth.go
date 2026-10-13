package restgw

import (
	"context"
	"net/http"
	"time"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// DPoPBindingCheck is the binding-only re-auth question for a DPoP stream:
// is accessToken still live and unrevoked, still bound to thumbprint, and is
// that device not revoked? nil means yes. A gRPC status of UNAUTHENTICATED or
// PERMISSION_DENIED, or a lib/dpop refusal, means no and ends the stream; any
// other error means the backend did not answer, and the stream stays open
// and is asked again sooner (as [WatchStreamAuth] does).
type DPoPBindingCheck func(ctx context.Context, accessToken, thumbprint string) error

// WatchStreamBinding is the stream re-auth watchdog for a stream set up
// through a [DPoPGate] or a bound ticket. It is [WatchStreamAuth] with the
// probe replaced: every interval it calls check with the stream's DPoP token
// (from r's `Authorization: DPoP` header, the promoted one for a ticket
// stream) and the thumbprint the stream was bound to — never replaying the
// set-up proof, which a jti store accepts once, or a spent ticket.
//
// The principal published on the watched context stays the one the stream
// was set up with: a binding check says yes or no, it does not re-derive the
// principal.
//
// A watched stream with no DPoP token on r or an empty thumbprint ends at
// once: there is no binding to check, and a binding watchdog that cannot ask
// its question must not keep the stream open. interval <= 0 or a nil check
// disables the watchdog, like [WatchStreamAuth].
func WatchStreamBinding(ctx context.Context, check DPoPBindingCheck, r *http.Request, thumbprint string, interval time.Duration) (context.Context, context.CancelFunc) {
	if interval <= 0 || check == nil || r == nil {
		return ctx, func() {}
	}
	token, ok := "", false
	if vals := r.Header.Values("Authorization"); len(vals) == 1 {
		token, ok = dpop.AccessToken(vals[0])
	}
	if !ok || thumbprint == "" {
		dead, cancel := context.WithCancel(ctx)
		cancel()
		return dead, cancel
	}
	userData := AuthUserDataFromContext(r.Context())
	probe := func(ctx context.Context, _ *http.Request) ([]byte, error) {
		if err := check(ctx, token, thumbprint); err != nil {
			return nil, err
		}
		return userData, nil
	}
	return watchStream(ctx, probe, r, interval, isBindingRefusal)
}

// isBindingRefusal is [isAuthRefusal] widened by the lib/dpop refusals a
// binding check may return (binding mismatch, invalid token).
func isBindingRefusal(err error) bool {
	return isAuthRefusal(err) || dpop.HTTPStatus(err) == http.StatusUnauthorized
}
