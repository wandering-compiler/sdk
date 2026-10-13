package restgw

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// WSAuthTicketThumbprintLabel is the ticket label holding the thumbprint a
// bound ticket was issued to ([DPoPTicketPrincipal]).
const WSAuthTicketThumbprintLabel = "dpop_jkt"

// ErrDPoPTicketUnverified is returned by [DPoPTicketPrincipal] for an issuer
// request whose DPoP-bound token and proof the gate did not verify.
var ErrDPoPTicketUnverified = errors.New("restgw: a bound ticket needs a verified DPoP-bound request")

// DPoPTicketPrincipal is the issuer Principal for bound tickets: the ticket
// exchange is a signed request. Mount the issuer behind [DPoPGate.Middleware]
// so the request's `Authorization: DPoP` token and proof are verified before
// this runs; it refuses ([ErrDPoPTicketUnverified]) any request the gate did
// not verify as bound, so an issuer mounted without the gate issues nothing.
//
// The ticket records the Authorization header (as [WSAuthTicketPrincipal]
// does, with the same size cap) and the verified thumbprint
// ([WSAuthTicketThumbprintLabel]); [NewDPoPWSAuth] checks the binding again
// when it redeems the ticket.
func DPoPTicketPrincipal(r *http.Request) (map[string]string, error) {
	v, ok := DPoPFromContext(r.Context())
	if !ok || !v.Bound() {
		return nil, ErrDPoPTicketUnverified
	}
	labels, err := WSAuthTicketPrincipal(r)
	if err != nil {
		return nil, err
	}
	labels[WSAuthTicketThumbprintLabel] = v.Thumbprint
	return labels, nil
}

// DPoPWSAuthConfig configures [NewDPoPWSAuth]: the [WSAuthConfig] fields
// plus the gate.
type DPoPWSAuthConfig struct {
	WSAuthConfig
	// Gate verifies header-mode upgrades and supplies the binding lookup
	// that redemption re-checks. Required.
	Gate *DPoPGate
}

// DPoPWSAuth is the stream auth for a DPoP surface. Use [DPoPWSAuth.AuthFunc]
// where [NewWSAuth]'s AuthFunc would go and [DPoPWSAuth.Thumbprint] to read
// the device the stream was bound to.
type DPoPWSAuth struct {
	cfg          DPoPWSAuthConfig
	acceptHeader bool
	acceptTicket bool
	param        string
	marker       string
}

// thumbprintHeader carries the thumbprint this wrapper established, beside
// the per-instance marker; both are written only by the wrapper, and a client
// copy is removed on entry.
const thumbprintHeader = "X-W17-Dpop-Jkt"

// NewDPoPWSAuth is [NewWSAuth] for a DPoP surface. Modes, the ticket store
// and the parameter are as there; the differences:
//
//   - Header mode: the upgrade's DPoP material is verified by the gate (or
//     taken from [DPoPFromContext] when the gate's middleware already ran);
//     a refusal is a [*DPoPError] (401 / 503 through [WriteAuthError]).
//   - Ticket mode: a ticket must carry [WSAuthTicketThumbprintLabel] and a
//     DPoP-scheme Authorization, and the token must still be bound to that
//     thumbprint ([DPoPBindingFunc]); otherwise the redemption is
//     [ErrTicketInvalid]. One-shot redemption stands in for the proof the
//     browser cannot send. An unbound ticket (from [WSAuthTicketPrincipal])
//     is accepted, with no device, only when the gate is not
//     [WithDPoPRequired].
//
// Panics on the same configuration errors as NewWSAuth, and on a nil Gate.
func NewDPoPWSAuth(cfg DPoPWSAuthConfig) *DPoPWSAuth {
	if cfg.Gate == nil {
		panic("restgw: DPoPWSAuthConfig.Gate is required")
	}
	// NewWSAuth validates the shared fields and panics the same way.
	_ = NewWSAuth(cfg.WSAuthConfig)
	a := &DPoPWSAuth{cfg: cfg, param: cfg.TicketParam, marker: randomMarker()}
	if a.param == "" {
		a.param = "ticket"
	}
	modes := cfg.Modes
	if len(modes) == 0 {
		modes = []string{WSAuthModeHeader}
	}
	for _, m := range modes {
		switch m {
		case WSAuthModeHeader:
			a.acceptHeader = true
		case WSAuthModeTicket:
			a.acceptTicket = true
		}
	}
	return a
}

// Thumbprint returns the device thumbprint this wrapper bound r's stream to,
// after its AuthFunc accepted r. A value a client sent is never returned.
func (a *DPoPWSAuth) Thumbprint(r *http.Request) (string, bool) {
	if r.Header.Get(promotedHeader) != a.marker {
		return "", false
	}
	t := r.Header.Get(thumbprintHeader)
	return t, t != ""
}

// AuthFunc returns the decorated AuthFunc.
func (a *DPoPWSAuth) AuthFunc() AuthFunc {
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
				return nil, ErrWSAuthNoTicket
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
func (a *DPoPWSAuth) viaHeader(ctx context.Context, r *http.Request) ([]byte, error) {
	v, ok := DPoPFromContext(r.Context())
	if !ok {
		var err error
		if v, err = a.cfg.Gate.Verify(r); err != nil {
			return nil, &DPoPError{Err: err}
		}
	}
	if v.Thumbprint == "" {
		return a.cfg.Inner(ctx, r)
	}
	return a.innerThenMark(ctx, r, v.Thumbprint)
}

// viaTicket redeems a ticket and checks its binding before Inner runs.
func (a *DPoPWSAuth) viaTicket(ctx context.Context, r *http.Request, ticket string) ([]byte, error) {
	labels, err := a.cfg.TicketStore.Redeem(ctx, ticket)
	if err != nil {
		return nil, err
	}
	auth := labels[WSAuthTicketHeaderLabel]
	if auth == "" {
		return nil, errors.New("restgw: ticket labels missing authorization")
	}
	thumb := labels[WSAuthTicketThumbprintLabel]
	if thumb == "" {
		if a.cfg.Gate.required {
			return nil, ErrTicketInvalid
		}
		r.Header.Set("Authorization", auth)
		return a.innerThenMark(ctx, r, "")
	}
	token, ok := dpop.AccessToken(auth)
	if !ok {
		return nil, ErrTicketInvalid
	}
	bound, err := a.cfg.Gate.lookupBinding(ctx, token)
	switch {
	case errors.Is(err, dpop.ErrUnavailable):
		return nil, &DPoPError{Err: err}
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrTicketInvalid, err)
	case subtle.ConstantTimeCompare([]byte(bound), []byte(thumb)) != 1:
		return nil, ErrTicketInvalid
	}
	r.Header.Set("Authorization", auth)
	return a.innerThenMark(ctx, r, thumb)
}

// innerThenMark runs Inner and, only when it accepts, marks r as accepted
// by this wrapper: the thumbprint (none for an unbound ticket) and the
// per-instance marker that lets [DPoPWSAuth.Thumbprint] and a later re-auth
// trust it. The caller has already put the credential on r (the ticket's
// Authorization is written onto the caller's request for the reason
// NewWSAuth gives). A refused request is left unmarked.
func (a *DPoPWSAuth) innerThenMark(ctx context.Context, r *http.Request, thumb string) ([]byte, error) {
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
