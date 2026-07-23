// Short-lived one-shot ticket store for browser auth on
// streaming endpoints (G3i3-GW-F). Adapted from protobridge's
// `runtime/events/ticket.go`.
//
// Why this exists: browsers' `new WebSocket()` and `EventSource`
// constructors CANNOT set custom headers — `Authorization:
// Bearer ...` doesn't reach the upgrade. The ticket flow:
//
//   1. FE POSTs to the issuer endpoint with its normal auth
//      header (Authorization / Cookie / X-API-Key).
//   2. Issuer's Principal function records the auth credentials
//      into the ticket's labels.
//   3. Issuer mints a random short-TTL ticket + returns
//      {"ticket":"...","expires_in":<seconds>} as JSON.
//   4. FE opens the WS / SSE URL with `?ticket=<t>` appended.
//   5. WS-auth wrapper redeems the ticket on the upgrade,
//      replays the recorded Authorization header into a
//      cloned request, and dispatches to the inner AuthFunc
//      so backend auth sees the same shape as a direct
//      header-authenticated upgrade.
//
// Tickets are one-shot (Redeem deletes) so a leaked ticket
// expires on first use. TTL bounds the replay window.
//
// Multi-replica deploys need a shared backend (Redis, DB)
// because issuer + redeemer can land on different pods. The
// in-memory default works for single-replica.

package restgw

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// ErrTicketInvalid is returned when the ticket is unknown,
// expired, or already consumed. All three indistinguishably
// — surfacing the reason would let an attacker probe ticket
// validity.
var ErrTicketInvalid = errors.New("restgw: ticket invalid or expired")

// TicketStore issues + redeems short-lived one-shot tickets.
// Implementations must guarantee: a successful Redeem makes
// the ticket invalid for any future Redeem (one-shot
// semantic).
type TicketStore interface {
	Issue(ctx context.Context, labels map[string]string, ttl time.Duration) (string, error)
	Redeem(ctx context.Context, ticket string) (map[string]string, error)
}

// NewMemoryTicketStore builds the in-process default. Background
// janitor evicts expired entries every minute. Call Close on
// shutdown to stop the janitor (optional — usable without).
func NewMemoryTicketStore() *MemoryTicketStore {
	s := &MemoryTicketStore{
		entries: make(map[string]memoryTicket),
		stop:    make(chan struct{}),
	}
	go s.janitor()
	return s
}

// MemoryTicketStore is the in-process TicketStore. Exported
// so single-replica main.go can construct + Close it
// explicitly.
type MemoryTicketStore struct {
	mu       sync.Mutex
	entries  map[string]memoryTicket
	stop     chan struct{}
	stopOnce sync.Once
}

type memoryTicket struct {
	labels    map[string]string
	expiresAt time.Time
}

// Issue mints a 32-byte URL-safe random ticket, stores it +
// returns the encoded form. ttl <= 0 → 30s default (browser
// fetch → WS-connect round-trip is comfortably under).
func (s *MemoryTicketStore) Issue(_ context.Context, labels map[string]string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf[:])

	// Defensive copy — caller mutating labels after Issue
	// must not affect the stored ticket.
	var copied map[string]string
	if labels != nil {
		copied = make(map[string]string, len(labels))
		for k, v := range labels {
			copied[k] = v
		}
	}
	s.mu.Lock()
	s.entries[ticket] = memoryTicket{labels: copied, expiresAt: time.Now().Add(ttl)}
	s.mu.Unlock()
	return ticket, nil
}

// Redeem looks up + removes the ticket atomically. Returns
// ErrTicketInvalid for unknown / expired / already-consumed.
// Callers map this to 401.
func (s *MemoryTicketStore) Redeem(_ context.Context, ticket string) (map[string]string, error) {
	if ticket == "" {
		return nil, ErrTicketInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[ticket]
	if !ok {
		return nil, ErrTicketInvalid
	}
	delete(s.entries, ticket)
	if time.Now().After(entry.expiresAt) {
		return nil, ErrTicketInvalid
	}
	return entry.labels, nil
}

// Close stops the background janitor. Idempotent; safe to
// call from a deferred shutdown block.
func (s *MemoryTicketStore) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *MemoryTicketStore) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.reapExpired(now)
		}
	}
}

func (s *MemoryTicketStore) reapExpired(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, k)
		}
	}
}

// TicketIssuerConfig wires the HTTP endpoint that exchanges
// auth credentials for a one-shot ticket. main.go template
// constructs this via the auth-method principal hook +
// MountTicketIssuer.
type TicketIssuerConfig struct {
	// Principal resolves labels for the incoming POST. The
	// canonical implementation reads the Authorization header
	// and stores it under the WSAuthTicketHeaderLabel key —
	// see WSAuthTicketPrincipal.
	Principal func(r *http.Request) (map[string]string, error)

	// Store mints + redeems tickets. Required.
	Store TicketStore

	// TTL bounds ticket validity. Defaults to 30s.
	TTL time.Duration
}

// NewTicketIssuer returns the http.Handler the issuer mounts
// at the configured path. Accepts only POST (other verbs →
// 405 with Allow header). Principal failure → 401; Store
// failure → 500.
func NewTicketIssuer(cfg TicketIssuerConfig) http.Handler {
	if cfg.Principal == nil {
		panic("restgw: TicketIssuerConfig.Principal is required")
	}
	if cfg.Store == nil {
		panic("restgw: TicketIssuerConfig.Store is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		labels, err := cfg.Principal(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ticket, err := cfg.Store.Issue(r.Context(), labels, cfg.TTL)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		// Vary on what we keyed the ticket on — defense in
		// depth if an upstream ever overrides Cache-Control.
		w.Header().Set("Vary", "Authorization, Cookie")
		_ = json.NewEncoder(w).Encode(struct {
			Ticket    string `json:"ticket"`
			ExpiresIn int    `json:"expires_in"`
		}{Ticket: ticket, ExpiresIn: int(cfg.TTL.Seconds())})
	})
}

// MountTicketIssuer creates an in-memory TicketStore + mounts
// the POST issuer at path on r. Returns the store so main.go
// can hand it to NewWSAuth + Close it on shutdown. Takes
// chi.Router so it composes with the rest of the gateway's
// route tree (REV-031 Phase C-6 fix-3 chi migration).
func MountTicketIssuer(r chi.Router, path string, principal func(*http.Request) (map[string]string, error)) *MemoryTicketStore {
	store := NewMemoryTicketStore()
	r.Method(http.MethodPost, path, NewTicketIssuer(TicketIssuerConfig{
		Principal: principal,
		Store:     store,
	}))
	return store
}
