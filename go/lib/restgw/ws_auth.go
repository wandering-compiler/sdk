// WS / SSE auth decorator (G3i3-GW-F). Adapted from
// protobridge's `runtime/ws_auth.go`.
//
// Wraps an existing AuthFunc to accept either:
//   - the standard Authorization header (works for native
//     clients, curl, server-to-server), OR
//   - a `?ticket=<t>` query parameter on the upgrade URL
//     (browser path — `new WebSocket()` / `EventSource`
//     cannot set custom headers).
//
// On ticket redemption, the recorded Authorization header is
// replayed into a cloned request before the inner AuthFunc
// runs, so backend auth sees the same shape it would for a
// direct header upgrade.

package restgw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// WSAuthMode enumerates accepted credential sources. Strings
// (not iota) so misconfiguration produces a clear startup
// error instead of a silent number mismatch.
const (
	WSAuthModeHeader = "header"
	WSAuthModeTicket = "ticket"
)

// WSAuthTicketHeaderLabel is the reserved ticket-label key
// the issuer's Principal MUST populate with the original
// Authorization header. NewWSAuth reads this exact key when
// redeeming + replays it as Authorization on the cloned
// request. Fixed on purpose — a configurable name is more
// footgun than flexibility.
const WSAuthTicketHeaderLabel = "authorization"

// WSAuthConfig configures NewWSAuth.
type WSAuthConfig struct {
	// Inner is the upstream AuthFunc that calls the auth RPC.
	// Required. NewWSAuth decorates it: on ticket redemption,
	// the replayed Authorization header lands on a cloned
	// request before Inner runs, so Inner's view is
	// indistinguishable from a header upgrade.
	Inner AuthFunc

	// TicketStore redeems one-shot tickets minted by
	// NewTicketIssuer. Required when Modes contains "ticket".
	TicketStore TicketStore

	// TicketParam is the query-string key carrying the ticket
	// on the upgrade URL. Defaults to "ticket".
	TicketParam string

	// Modes is the credential-source allow-list. Empty →
	// header-only (caller opts into ticket redemption
	// explicitly). Unknown values panic at NewWSAuth time
	// so typos surface before serving.
	Modes []string
}

// ErrWSAuthNoTicket is returned when Modes is ticket-only +
// the request carries no ticket. Sentinel so callers can
// distinguish "client forgot ticket" (401) from "TicketStore
// transport blew up" (500).
var ErrWSAuthNoTicket = errors.New("restgw: ticket required but not supplied")

// ErrWSAuthTicketNoHeader is returned by WSAuthTicketPrincipal
// when the issuer request lacks Authorization — nothing to
// record, so issuing a ticket would produce an unusable one.
var ErrWSAuthTicketNoHeader = errors.New("restgw: ticket issuer requires Authorization header")

// WSAuthTicketPrincipal is the canonical Principal for
// NewTicketIssuer when paired with NewWSAuth: records the
// incoming Authorization header into the ticket's labels so
// NewWSAuth can replay it on redemption. Tickets are issued
// only when the header is present; actual auth validation
// happens at redeem time via the wrapped AuthFunc, so this
// Principal deliberately does NOT call the upstream auth
// service — the issuer is a thin "trade your header for a
// ticket" trampoline.
func WSAuthTicketPrincipal(r *http.Request) (map[string]string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, ErrWSAuthTicketNoHeader
	}
	return map[string]string{WSAuthTicketHeaderLabel: auth}, nil
}

// NewWSAuth decorates an AuthFunc with ticket-redemption
// support. Modes acts as an allow-list:
//
//   - "header"          → Authorization passed through to
//     Inner. Tickets ignored.
//   - "ticket"          → only ticket redemption; no-ticket
//     requests fail fast with
//     ErrWSAuthNoTicket so a stray
//     Authorization header cannot leak
//     to Inner behind the caller's
//     back.
//   - "header,ticket"   → Authorization wins when set;
//     otherwise ticket is redeemed +
//     labels[WSAuthTicketHeaderLabel] is
//     replayed as Authorization on a
//     cloned request before Inner runs.
//
// When both sources are accepted but neither is present,
// the request is forwarded to Inner unchanged so Inner's
// own missing-credentials branch produces the 401.
//
// Panics on Inner==nil, unknown Modes value, or ticket mode
// without TicketStore — config errors that should surface at
// startup, not on the first WS upgrade.
func NewWSAuth(cfg WSAuthConfig) AuthFunc {
	if cfg.Inner == nil {
		panic("restgw: WSAuthConfig.Inner is required")
	}
	modes := cfg.Modes
	if len(modes) == 0 {
		modes = []string{WSAuthModeHeader}
	}
	acceptHeader, acceptTicket := false, false
	for _, m := range modes {
		switch m {
		case WSAuthModeHeader:
			acceptHeader = true
		case WSAuthModeTicket:
			acceptTicket = true
		default:
			panic(fmt.Sprintf("restgw: unknown WSAuth mode %q", m))
		}
	}
	if acceptTicket && cfg.TicketStore == nil {
		panic("restgw: WSAuthConfig.TicketStore is required when Modes includes \"ticket\"")
	}
	param := cfg.TicketParam
	if param == "" {
		param = "ticket"
	}

	return func(ctx context.Context, r *http.Request) ([]byte, error) {
		if acceptHeader && r.Header.Get("Authorization") != "" {
			return cfg.Inner(ctx, r)
		}
		if acceptTicket {
			ticket := r.URL.Query().Get(param)
			if ticket == "" {
				if !acceptHeader {
					return nil, ErrWSAuthNoTicket
				}
				// Header+ticket allowed, neither present →
				// let Inner produce its standard
				// missing-credentials 401.
				return cfg.Inner(ctx, r)
			}
			labels, err := cfg.TicketStore.Redeem(ctx, ticket)
			if err != nil {
				return nil, err
			}
			auth := labels[WSAuthTicketHeaderLabel]
			if auth == "" {
				return nil, errors.New("restgw: ticket labels missing authorization")
			}
			// r.Clone deep-copies the Header map; safe to
			// mutate the clone without touching the caller's
			// request.
			replayed := r.Clone(ctx)
			replayed.Header.Set("Authorization", auth)
			return cfg.Inner(ctx, replayed)
		}
		return cfg.Inner(ctx, r)
	}
}
