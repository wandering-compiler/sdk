// URL-carried credentials (REV-162) — the runtime half of
// `RestEndpoint.credential` / `TOKEN_TYPE_URL_TOKEN`.
//
// A capability link ("open your invoice") has a perfectly good
// credential; it just arrives in the URL, because the recipient
// has no session and a browser following a link cannot set a
// header. The platform answer is NOT to exclude such an endpoint
// from auth — that would throw away scopes, ACL, logging and
// every future per-principal mechanic, and push tenant isolation
// onto a hand-written WHERE clause. It is to accept the
// credential from the URL and run the ordinary auth path with it.
//
// Three pieces live here:
//
//   - [CredentialHeader] — the reserved slot the extracted value
//     is carried in, so the auth backend contract (AuthReq's
//     `map<string,string> headers`) does not change at all. A
//     hand-written auth service reads it exactly like the auth
//     plugin would.
//   - [StripReservedHeadersMiddleware] — deletes that slot from
//     every inbound request, unconditionally, before any other
//     layer observes it. Without this the "reserved" slot is just
//     a header a client can set, and the whole channel is
//     forgeable. It is deliberately NOT conditional on the
//     surface declaring a URL_TOKEN method: the header must be
//     unspoofable everywhere, including on surfaces that gain
//     such a method later.
//   - [WithURLCredential] + the extractors — the per-endpoint
//     injection the generated handler performs before calling
//     the surface auth function.
//
// The injection doubles as the routing signal: because inbound
// requests are scrubbed, the presence of [CredentialHeader] at
// auth time proves the gateway itself put it there for an
// endpoint that declared `credential`, so the auth router can
// dispatch to the URL_TOKEN method on that alone. Signal and
// authorisation-to-use-it are the same fact, which is what keeps
// `?token=<stolen>` on some OTHER route from authenticating.
package restgw

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CredentialHeader is the reserved request-header slot carrying a
// URL-extracted credential into the auth call. Canonical form, as
// stored in http.Header.
//
// Fixed on purpose, like [WSAuthTicketHeaderLabel]: a configurable
// name would mean the scrub list and the injection could disagree,
// and a scrub that misses is indistinguishable from one that works
// until someone forges the header.
const CredentialHeader = "X-W17-Credential" // #nosec G101 -- the NAME of a header slot, not a credential value; the secret is what the gateway puts in it

// CredentialMetadataKey is the lower-cased form of
// [CredentialHeader] — the key the auth backend actually looks up
// in `AuthReq.headers`, which is lower-case-normalised.
const CredentialMetadataKey = "x-w17-credential" // #nosec G101 -- lower-cased form of the same header NAME; see above

// ErrMissingURLCredential is returned when an endpoint declares a
// `credential` slot but the request carries no value in it — an
// empty path segment or an absent query parameter.
//
// Deliberately Unauthenticated (401) rather than InvalidArgument
// (400): from the caller's side "no token" and "wrong token" are
// the same event, and answering them differently lets an attacker
// distinguish a malformed link from a revoked one.
var ErrMissingURLCredential = status.Error(codes.Unauthenticated, "missing credential")

// reservedInboundHeaders lists every header the gateway sets for
// itself and must therefore never accept from a client.
//
// Add to this list — do not add a second scrub elsewhere. A
// forgeable-channel bug is silent, and the way it usually arrives
// is a second copy of the guard that a later reader mistakes for
// the only one.
var reservedInboundHeaders = []string{CredentialHeader}

// StripReservedHeadersMiddleware removes gateway-reserved headers
// from the inbound request before passing it down the chain.
//
// Mount it OUTSIDE everything that reads headers — metadata
// propagation, observability, the handlers themselves — so no
// layer can observe a client-supplied value. The generated main
// wraps it directly inside the panic recovery, i.e. the first
// thing after the request enters the API chain.
//
// Requests almost never carry these headers, so the common path
// is a lookup miss and the original request is passed through
// untouched; only a request that actually carries one pays for a
// header-map clone.
func StripReservedHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		present := false
		for _, name := range reservedInboundHeaders {
			if _, ok := r.Header[name]; ok {
				present = true
				break
			}
		}
		if !present {
			next.ServeHTTP(w, r)
			return
		}
		// Clone rather than delete in place: r.Header may be
		// shared with a caller that recorded it (httptest, a
		// wrapping middleware holding its own reference), and
		// mutating a map through a shared reference is the kind
		// of aliasing bug that only shows under a specific test
		// ordering.
		scrubbed := r.Clone(r.Context())
		for _, name := range reservedInboundHeaders {
			scrubbed.Header.Del(name)
		}
		next.ServeHTTP(w, scrubbed)
	})
}

// URLCredentialFromPath reads the credential out of a path
// placeholder — `/public/invoices/{token}` with name "token".
//
// Returns "" when the segment is absent or empty; callers answer
// that with [ErrMissingURLCredential] rather than calling auth
// with a blank credential, so an auth backend can never be handed
// "" and accidentally match a row whose token column is empty.
func URLCredentialFromPath(r *http.Request, name string) string {
	if r == nil {
		return ""
	}
	return chi.URLParam(r, name)
}

// URLCredentialFromQuery reads the credential out of a query
// parameter — `?t=…` with name "t". Same empty-value contract as
// [URLCredentialFromPath].
func URLCredentialFromQuery(r *http.Request, name string) string {
	if r == nil {
		return ""
	}
	return r.URL.Query().Get(name)
}

// WithURLCredential returns a copy of r carrying value in the
// reserved [CredentialHeader] slot, for the auth call to consume.
//
// A copy, not a mutation: the credential must not be visible to
// anything that captured the request earlier in the chain, and the
// clone also gives the auth cache a stable header set to key on
// (see [defaultAuthCacheKeyHeaders], which includes this slot —
// without it two different capability tokens would collide on one
// cache entry and be served each other's identity).
func WithURLCredential(r *http.Request, value string) *http.Request {
	cloned := r.Clone(r.Context())
	cloned.Header.Set(CredentialHeader, value)
	return cloned
}

// HasURLCredential reports whether the request carries an injected
// URL credential — the signal the generated auth router uses to
// dispatch to the surface's URL_TOKEN method instead of
// classifying the Authorization scheme.
//
// Trustworthy only because [StripReservedHeadersMiddleware] runs
// first: a true answer means the gateway put it there.
func HasURLCredential(r *http.Request) bool {
	return r != nil && r.Header.Get(CredentialHeader) != ""
}

// IsMissingURLCredential reports whether err is the sentinel from
// [ErrMissingURLCredential], for callers that want to distinguish
// "the link was malformed" from "the backend rejected the token"
// in their own logging. The HTTP answer is identical either way.
func IsMissingURLCredential(err error) bool {
	return errors.Is(err, ErrMissingURLCredential)
}
