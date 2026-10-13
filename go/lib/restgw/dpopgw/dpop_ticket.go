package dpopgw

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/restgw"
)

// TicketThumbprintLabel is the ticket label holding the thumbprint a
// bound ticket was issued to ([TicketPrincipal]).
const TicketThumbprintLabel = "dpop_jkt"

// ErrTicketUnverified is returned by [TicketPrincipal] for an issuer
// request whose DPoP-bound token and proof the gate did not verify.
var ErrTicketUnverified = errors.New("dpopgw: a bound ticket needs a verified DPoP-bound request")

// TicketPrincipal is the issuer Principal for bound tickets: the ticket
// exchange is a signed request. Mount the issuer behind [Gate.Middleware]
// so the request's `Authorization: DPoP` token and proof are verified before
// this runs; it refuses ([ErrTicketUnverified]) any request the gate did
// not verify as bound, so an issuer mounted without the gate issues nothing.
//
// The ticket records the Authorization header (as [restgw.WSAuthTicketPrincipal]
// does, with the same size cap) and the verified thumbprint
// ([TicketThumbprintLabel]); [NewWSAuth] checks the binding again
// when it redeems the ticket.
func TicketPrincipal(r *http.Request) (map[string]string, error) {
	v, ok := FromContext(r.Context())
	if !ok || !v.Bound() {
		return nil, ErrTicketUnverified
	}
	labels, err := restgw.WSAuthTicketPrincipal(r)
	if err != nil {
		return nil, err
	}
	labels[TicketThumbprintLabel] = v.Thumbprint
	return labels, nil
}

// WSAuthConfig configures [NewWSAuth]: the restgw.WSAuthConfig fields
// plus the gate.
type WSAuthConfig struct {
	restgw.WSAuthConfig
	// Gate verifies header-mode upgrades and supplies the binding lookup
	// that redemption re-checks. Required.
	Gate *Gate
}

// WSAuth is the stream auth for a DPoP surface. Use [WSAuth.AuthFunc]
// where restgw.NewWSAuth's AuthFunc would go and [WSAuth.Thumbprint] to read
// the device the stream was bound to.
type WSAuth struct {
	cfg          WSAuthConfig
	acceptHeader bool
	acceptTicket bool
	param        string
	marker       string
}

// thumbprintHeader carries the thumbprint this wrapper established, beside
// the per-instance marker; both are written only by the wrapper, and a client
// copy is removed on entry.
const thumbprintHeader = "X-W17-Dpop-Jkt"

// NewWSAuth is [restgw.NewWSAuth] for a DPoP surface. Modes, the ticket store
// and the parameter are as there; the differences:
//
//   - Header mode: the upgrade's DPoP material is verified by the gate (or
//     taken from [FromContext] when the gate's middleware already ran);
//     a refusal is a [*Error] (401 / 503 through [restgw.WriteAuthError]).
//     Only a token-bound proof marks the stream with a device; an unbound
//     proof (a key the client may have just made) marks nothing.
//   - Ticket mode: a ticket must carry [TicketThumbprintLabel] and a
//     DPoP-scheme Authorization, and the token must still be bound to that
//     thumbprint ([BindingFunc]); otherwise the redemption is
//     [restgw.ErrTicketInvalid]. One-shot redemption stands in for the proof the
//     browser cannot send. An unbound ticket (from [restgw.WSAuthTicketPrincipal])
//     is accepted, with no device, only when the gate is not
//     [WithRequired] and its credential is not a DPoP token.
//
// Panics on the same configuration errors as restgw.NewWSAuth, and on a nil Gate.
func NewWSAuth(cfg WSAuthConfig) *WSAuth {
	if cfg.Gate == nil {
		panic("dpopgw: WSAuthConfig.Gate is required")
	}
	// NewWSAuth validates the shared fields and panics the same way.
	_ = restgw.NewWSAuth(cfg.WSAuthConfig)
	a := &WSAuth{cfg: cfg, param: cfg.TicketParam, marker: randomMarker()}
	if a.param == "" {
		a.param = "ticket"
	}
	modes := cfg.Modes
	if len(modes) == 0 {
		modes = []string{restgw.WSAuthModeHeader}
	}
	for _, m := range modes {
		switch m {
		case restgw.WSAuthModeHeader:
			a.acceptHeader = true
		case restgw.WSAuthModeTicket:
			a.acceptTicket = true
		}
	}
	return a
}

// Thumbprint returns the device thumbprint this wrapper bound r's stream to,
// after its AuthFunc accepted r: only a token-bound proof or a bound ticket
// binds a stream. A value a client sent is never returned.
func (a *WSAuth) Thumbprint(r *http.Request) (string, bool) {
	if r.Header.Get(promotedHeader) != a.marker {
		return "", false
	}
	t := r.Header.Get(thumbprintHeader)
	return t, t != ""
}

// Verified returns what this wrapper established for r's stream, for
// [SetDeviceMetadata]: the bound thumbprint and the DPoP token it is bound
// to. ok is false when the stream is not bound to a device.
func (a *WSAuth) Verified(r *http.Request) (Verified, bool) {
	thumb, ok := a.Thumbprint(r)
	if !ok {
		return Verified{}, false
	}
	token, ok := dpop.AccessToken(r.Header.Get("Authorization"))
	if !ok {
		return Verified{}, false
	}
	return Verified{Thumbprint: thumb, AccessToken: token}, true
}

// AuthFunc returns the decorated AuthFunc.
func (a *WSAuth) AuthFunc() restgw.AuthFunc {
	return func(ctx context.Context, r *http.Request) ([]byte, error) {
		if r.Header.Get(promotedHeader) == a.marker {
			// A request this wrapper already accepted: a stream's re-auth.
			return a.cfg.Inner(ctx, r)
		}
		// Never trust a client's copy of what only this wrapper writes.
		r.Header.Del(promotedHeader)
		r.Header.Del(thumbprintHeader)

		headerCred := r.Header.Get("Authorization") != "" || len(r.Header.Values(dpop.HeaderProof)) > 0
		if a.acceptHeader && headerCred {
			return a.viaHeader(ctx, r)
		}
		if !a.acceptTicket {
			return a.viaHeader(ctx, r)
		}
		ticket := r.URL.Query().Get(a.param)
		if ticket == "" {
			if !a.acceptHeader {
				return nil, restgw.ErrWSAuthNoTicket
			}
			// Neither present: the gate (required or not) and then Inner
			// produce the refusal.
			return a.viaHeader(ctx, r)
		}
		return a.viaTicket(ctx, r, ticket)
	}
}

// viaHeader authenticates an upgrade by its own headers: the gate's verdict
// (from the middleware, or verified here), then Inner.
func (a *WSAuth) viaHeader(ctx context.Context, r *http.Request) ([]byte, error) {
	v, ok := FromContext(r.Context())
	if !ok {
		var err error
		if v, err = a.cfg.Gate.Verify(r); err != nil {
			return nil, &Error{Err: err}
		}
	}
	if !v.Bound() {
		// No proof, or an unbound one: a key the client may have just made
		// is no device, so the stream is not marked with it.
		return a.cfg.Inner(ctx, r)
	}
	return a.innerThenMark(ctx, r, v.Thumbprint)
}

// viaTicket redeems a ticket and checks its binding before Inner runs.
func (a *WSAuth) viaTicket(ctx context.Context, r *http.Request, ticket string) ([]byte, error) {
	labels, err := a.cfg.TicketStore.Redeem(ctx, ticket)
	if err != nil {
		return nil, err
	}
	auth := labels[restgw.WSAuthTicketHeaderLabel]
	if auth == "" {
		return nil, errors.New("dpopgw: ticket labels missing authorization")
	}
	thumb := labels[TicketThumbprintLabel]
	if thumb == "" {
		// A DPoP token in a ticket without a thumbprint did not come from the
		// bound issuer; replaying it unbound would skip its binding.
		if a.cfg.Gate.required || isDPoPScheme(auth) {
			return nil, restgw.ErrTicketInvalid
		}
		r.Header.Set("Authorization", auth)
		return a.innerThenMark(ctx, r, "")
	}
	token, ok := dpop.AccessToken(auth)
	if !ok {
		return nil, restgw.ErrTicketInvalid
	}
	bound, err := a.cfg.Gate.lookupBinding(ctx, token)
	switch {
	case errors.Is(err, dpop.ErrUnavailable):
		return nil, &Error{Err: err}
	case err != nil:
		return nil, fmt.Errorf("%w: %w", restgw.ErrTicketInvalid, err)
	case subtle.ConstantTimeCompare([]byte(bound), []byte(thumb)) != 1:
		return nil, restgw.ErrTicketInvalid
	}
	r.Header.Set("Authorization", auth)
	return a.innerThenMark(ctx, r, thumb)
}

// innerThenMark runs Inner and, only when it accepts, marks r as accepted
// by this wrapper: the thumbprint (none for an unbound ticket) and the
// per-instance marker that lets [WSAuth.Thumbprint] and a later re-auth
// trust it. The caller has already put the credential on r (the ticket's
// Authorization is written onto the caller's request for the reason
// restgw.NewWSAuth gives). A refused request is left unmarked.
func (a *WSAuth) innerThenMark(ctx context.Context, r *http.Request, thumb string) ([]byte, error) {
	data, err := a.cfg.Inner(ctx, r)
	if err != nil {
		return nil, err
	}
	if thumb != "" {
		r.Header.Set(thumbprintHeader, thumb)
	}
	r.Header.Set(promotedHeader, a.marker)
	return data, nil
}

// promotedHeader carries this wrapper's per-instance marker, written only by
// the wrapper (a client copy is removed on entry, and the edge scrub drops the
// x-w17- namespace before that).
const promotedHeader = "X-W17-Dpop-Promoted"

// randomMarker mints the per-instance marker: 256 random bits. crypto/rand
// never returns an error (it aborts the process instead).
func randomMarker() string {
	var buf [32]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
