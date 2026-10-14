// Package dpopgw is DPoP at the REST edge: opt-in proof verification for
// RFC 9449 device-bound credentials, on top of lib/restgw.
//
// It is a package of its own so that lib/restgw imports nothing of lib/dpop:
// a gateway that does not use DPoP does not even link it.
//
// Nothing here runs unless a gateway mounts it. A gateway that never builds a
// [Gate] behaves exactly as before: bearer requests, the auth cache, the
// ticket flow and the stream watchdog are untouched. The pieces, in the order
// a request meets them:
//
//   - [Gate.Middleware] verifies `Authorization: DPoP <token>` + `DPoP:
//     <proof>` with lib/dpop, answers a refusal with the RFC 9449 challenge
//     (status from [dpop.HTTPStatus]), and records the verified thumbprint
//     on the request context ([FromContext]).
//   - [CachedAuthFunc] is the auth cache for such a surface: a verified
//     request is keyed on its credential AND the verified thumbprint, and a
//     request that carries DPoP material but was not verified is never
//     answered from the cache.
//   - [SetDeviceMetadata] stamps the verified thumbprint as the device id
//     (`x-w17-device`, read by principal.DeviceID) on the backend call.
//   - [TicketPrincipal] + [NewWSAuth] bind a stream ticket to the
//     thumbprint of the signed request that obtained it; redeeming it
//     re-checks the binding, and the one-shot redemption stands in for the
//     proof a browser WebSocket / EventSource cannot send.
//   - [WatchStreamBinding] is the stream watchdog for a DPoP stream: it asks
//     a binding-only question (token live, not revoked, device not revoked)
//     instead of replaying a proof that can be presented only once.
package dpopgw

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/principal"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// BindingFunc returns the thumbprint an access token is bound to (its
// `cnf.jkt`), as the auth backend knows it. The gate compares the proof's key
// with it. Return "" for a token that is not DPoP-bound, a gRPC status of
// UNAUTHENTICATED or PERMISSION_DENIED (or a dpop refusal) for a token that is
// unknown, expired or revoked, and any other error when the backend could not
// answer: those become 503, never an acceptance.
type BindingFunc func(ctx context.Context, accessToken string) (string, error)

// Gate verifies DPoP proofs at the REST edge. Build it with [NewGate].
// It is safe for concurrent use.
type Gate struct {
	v        *dpop.Verifier
	binding  BindingFunc
	required bool
}

// Option configures [NewGate].
type Option func(*Gate)

// WithBinding sets the token → thumbprint lookup. Without it every
// `Authorization: DPoP` request is refused (fail closed): a gate that cannot
// say which key a token is bound to cannot accept it.
func WithBinding(fn BindingFunc) Option {
	return func(g *Gate) { g.binding = fn }
}

// WithRequired refuses a request that carries no DPoP proof at all, with
// the bare `WWW-Authenticate: DPoP` challenge. Without it a request with
// neither a proof nor a DPoP token passes through unverified (a bearer or
// cookie request), and only DPoP material that is present is checked.
func WithRequired() Option {
	return func(g *Gate) { g.required = true }
}

// NewGate returns a gate verifying with v. v is required.
func NewGate(v *dpop.Verifier, opts ...Option) (*Gate, error) {
	if v == nil {
		return nil, errors.New("dpopgw: NewGate needs a verifier")
	}
	g := &Gate{v: v}
	for _, o := range opts {
		o(g)
	}
	return g, nil
}

// Verified is what the gate established for one request.
type Verified struct {
	// Thumbprint is the RFC 7638 thumbprint of the key whose proof verified.
	// On its own it proves only possession of a key — any client can make
	// one — so it is a device id only when the proof was bound to a token
	// ([Verified.DeviceID]).
	Thumbprint string
	// AccessToken is the DPoP-bound token the proof was checked against, or
	// "" for an unbound proof (a mint request: no token yet).
	AccessToken string
	// MintUnrecorded is true when the unbound proof was accepted without its
	// jti being recorded, because the mint store was full and the verifier
	// is configured with [dpop.MintFullAcceptUnrecorded]
	// ([dpop.Result.MintUnrecorded]). The proof could be presented again
	// while the store stays full; a mint handler should log it.
	MintUnrecorded bool
}

// Bound reports whether the proof was checked against a token's binding.
func (v Verified) Bound() bool { return v.AccessToken != "" }

// DeviceID is the device id this verification establishes: the thumbprint
// of a proof bound to a token, and "" for an unbound (mint) proof, whose key
// the client may have generated a moment ago. A mint endpoint that needs the
// new key's thumbprint to bind a token reads Thumbprint; nothing else should.
func (v Verified) DeviceID() string {
	if !v.Bound() {
		return ""
	}
	return v.Thumbprint
}

type verifiedKey struct{}

// FromContext returns what [Gate.Middleware] verified for this
// request. Only the gate can put it there (the context key is private), so
// ok means the proof was verified on this request.
func FromContext(ctx context.Context) (Verified, bool) {
	v, ok := ctx.Value(verifiedKey{}).(Verified)
	return v, ok && v.Thumbprint != ""
}

func withVerified(ctx context.Context, v Verified) context.Context {
	return context.WithValue(ctx, verifiedKey{}, v)
}

// Verify checks the DPoP material on r and returns what it established. A
// request with no proof and no DPoP token returns the zero value and nil —
// unless the gate is [WithRequired], which refuses it with
// [dpop.ErrMissingProof]. Every error is a lib/dpop error: [dpop.HTTPStatus]
// and [dpop.SetChallenge] answer it.
//
// The rules beyond [dpop.Verifier.Verify]:
//
//   - `Authorization: DPoP <token>` needs a proof and a binding: the proof's
//     key must be the one [BindingFunc] reports for the token.
//   - A proof with no Authorization is an unbound (mint) proof.
//   - A proof next to any other Authorization scheme, and more than one
//     Authorization header with a DPoP one among them, are malformed: a
//     proof never vouches for a credential it was not checked against.
func (g *Gate) Verify(r *http.Request) (Verified, error) {
	ctx := r.Context()
	proofs := r.Header.Values(dpop.HeaderProof)
	authz := r.Header.Values("Authorization")
	token, isDPoP := "", false
	for _, a := range authz {
		if t, ok := dpop.AccessToken(a); ok {
			if isDPoP || len(authz) > 1 {
				return Verified{}, fmt.Errorf("%w: a DPoP token among several Authorization headers", dpop.ErrMalformed)
			}
			token, isDPoP = t, true
		}
	}
	switch {
	case !isDPoP && len(proofs) == 0:
		if g.required {
			return Verified{}, dpop.ErrMissingProof
		}
		return Verified{}, nil
	case !isDPoP && len(authz) > 0:
		return Verified{}, fmt.Errorf("%w: a proof next to a non-DPoP Authorization scheme", dpop.ErrMalformed)
	}
	binding := ""
	if isDPoP {
		if len(proofs) == 0 {
			return Verified{}, dpop.ErrMissingProof
		}
		var err error
		if binding, err = g.lookupBinding(ctx, token); err != nil {
			return Verified{}, err
		}
	}
	res, err := g.v.VerifyResult(ctx, dpop.Request{
		Method:      r.Method,
		Path:        r.URL.EscapedPath(),
		Proofs:      proofs,
		AccessToken: token,
		Binding:     binding,
	})
	if err != nil {
		return Verified{}, err
	}
	return Verified{Thumbprint: res.Thumbprint, AccessToken: token, MintUnrecorded: res.MintUnrecorded}, nil
}

// lookupBinding asks the binding func which key token is bound to, mapping
// its answer onto the lib/dpop error vocabulary.
func (g *Gate) lookupBinding(ctx context.Context, token string) (string, error) {
	if g.binding == nil {
		return "", fmt.Errorf("%w: no binding lookup configured", dpop.ErrBindingMismatch)
	}
	b, err := g.binding(ctx, token)
	switch {
	case err == nil && b == "":
		return "", fmt.Errorf("%w: the token is not DPoP-bound", dpop.ErrBindingMismatch)
	case err == nil:
		return b, nil
	case dpop.HTTPStatus(err) == http.StatusUnauthorized || isAuthRefusal(err):
		return "", fmt.Errorf("%w: %w", dpop.ErrBindingMismatch, err)
	}
	return "", fmt.Errorf("%w: binding lookup: %w", dpop.ErrUnavailable, err)
}

// Middleware verifies every request through the gate. A refusal is answered
// here — the challenge headers ([dpop.SetChallenge]: WWW-Authenticate,
// DPoP-Nonce, Cache-Control, or Retry-After for a 503) and the standard error
// envelope with the status [dpop.HTTPStatus] gives — and next never runs. A
// verified request reaches next with [FromContext] set and, when the
// verifier issues nonces, a fresh `DPoP-Nonce` on the response for the
// client's next proof. A request the gate lets through unverified (no DPoP
// material, gate not required) reaches next unchanged.
func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := g.Verify(r)
		if err != nil {
			writeRefusal(r.Context(), w, err)
			return
		}
		if v.Thumbprint == "" {
			next.ServeHTTP(w, r)
			return
		}
		if n, err := g.v.Nonce(v.Thumbprint); err == nil {
			w.Header().Set(dpop.HeaderNonce, n)
		}
		next.ServeHTTP(w, r.WithContext(withVerified(r.Context(), v)))
	})
}

// writeRefusal answers a gate refusal: challenge headers, then the
// envelope with the code matching the status.
func writeRefusal(ctx context.Context, w http.ResponseWriter, err error) {
	dpop.SetChallenge(w.Header(), err)
	switch c := dpop.GRPCCode(err); c {
	case codes.Unauthenticated, codes.Unavailable:
		restgw.WriteRefusal(ctx, w, c, "dpop: "+err.Error())
	default:
		w.Header().Del(dpop.HeaderWWWAuthenticate)
		restgw.WriteInternal(ctx, w, "dpop: "+err.Error())
	}
}

// Error carries a lib/dpop error through an [restgw.AuthFunc]. Its gRPC status
// is [dpop.GRPCCode] of the cause, so [restgw.WriteAuthError] answers 401 / 503 and
// the stream watchdog reads a refusal as a refusal; errors.Is reaches the
// cause. An AuthFunc cannot set response headers, so the challenge itself is
// only sent by [Gate.Middleware].
type Error struct{ Err error }

func (e *Error) Error() string { return e.Err.Error() }

// Unwrap returns the lib/dpop cause.
func (e *Error) Unwrap() error { return e.Err }

// GRPCStatus maps the cause onto Unauthenticated / Unavailable / Internal.
func (e *Error) GRPCStatus() *status.Status {
	return status.New(dpop.GRPCCode(e.Err), "dpop: "+e.Err.Error())
}

// SetDeviceMetadata stamps the device id of v ([Verified.DeviceID]) on
// ctx's outgoing metadata (principal.DeviceMetadataKey), next to
// [restgw.SetUserMetadata]'s envelope. Only a proof bound to a token yields a
// device id: for an unbound proof, or the zero value, any copy is removed, so
// a key a client just generated never becomes a device. Pass what the gate
// verified for this request ([FromContext], [WSAuth.Verified]).
func SetDeviceMetadata(ctx context.Context, v Verified) context.Context {
	md, ok := metadata.FromOutgoingContext(ctx)
	if ok {
		md = md.Copy()
	} else {
		md = metadata.MD{}
	}
	principal.SetDeviceID(md, v.DeviceID())
	return metadata.NewOutgoingContext(ctx, md)
}
