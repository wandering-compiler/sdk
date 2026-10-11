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
//      sets the recorded Authorization header on the request,
//      and dispatches to the inner AuthFunc so backend auth
//      sees the same shape as a direct header-authenticated
//      upgrade. The header stays on the request: a stream is
//      re-probed for its whole life, and the ticket is spent.
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
	"google.golang.org/grpc/codes"
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

// ErrTicketStoreFull is returned by Issue when the store is at its entry or
// byte limit and sweeping expired tickets freed nothing. The issuer answers
// 503 with Retry-After: tickets live for seconds, so capacity returns quickly.
var ErrTicketStoreFull = errors.New("restgw: ticket store full")

// Default limits of the in-memory store. The issuer mints a ticket for any
// request that carries a credential, before that credential is validated
// (validation happens at redeem time), so the store must not trust its
// callers to keep it small: both the number of outstanding tickets and the
// bytes their labels hold are capped.
const (
	// DefaultMaxTickets bounds outstanding (unredeemed, unexpired) tickets.
	DefaultMaxTickets = 10000
	// DefaultMaxTicketBytes bounds the memory accounted to outstanding
	// tickets: ticket id + label keys and values + a fixed per-entry
	// overhead (ticketEntryOverhead).
	DefaultMaxTicketBytes = 16 << 20
)

// ticketEntryOverhead approximates the per-entry cost that is not label
// payload (map slot, struct, timestamp, label map header), so many tiny
// tickets still count against the byte budget.
const ticketEntryOverhead = 128

// MemoryTicketStoreOption configures NewMemoryTicketStore.
type MemoryTicketStoreOption func(*MemoryTicketStore)

// WithMaxTickets sets the outstanding-ticket cap. n <= 0 keeps the default.
func WithMaxTickets(n int) MemoryTicketStoreOption {
	return func(s *MemoryTicketStore) {
		if n > 0 {
			s.maxEntries = n
		}
	}
}

// WithMaxTicketBytes sets the byte budget for outstanding tickets. n <= 0
// keeps the default.
func WithMaxTicketBytes(n int64) MemoryTicketStoreOption {
	return func(s *MemoryTicketStore) {
		if n > 0 {
			s.maxBytes = n
		}
	}
}

// NewMemoryTicketStore builds the in-process default. Background
// janitor evicts expired entries every minute; Issue also sweeps
// expired entries itself when the store is at a limit. Call Close on
// shutdown to stop the janitor (optional — usable without).
//
// Limits: at most DefaultMaxTickets outstanding tickets holding at most
// DefaultMaxTicketBytes (override with WithMaxTickets / WithMaxTicketBytes).
// When full, Issue refuses with ErrTicketStoreFull rather than evicting:
// evicting would let new requests cancel tickets already handed to other
// clients, while refusing only delays new ones until the TTL frees room.
func NewMemoryTicketStore(opts ...MemoryTicketStoreOption) *MemoryTicketStore {
	s := &MemoryTicketStore{
		entries:    make(map[string]memoryTicket),
		stop:       make(chan struct{}),
		maxEntries: DefaultMaxTickets,
		maxBytes:   DefaultMaxTicketBytes,
		now:        time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	go s.janitor()
	return s
}

// MemoryTicketStore is the in-process TicketStore. Exported
// so single-replica main.go can construct + Close it
// explicitly.
type MemoryTicketStore struct {
	mu         sync.Mutex
	entries    map[string]memoryTicket
	bytes      int64
	maxEntries int
	maxBytes   int64
	now        func() time.Time
	stop       chan struct{}
	stopOnce   sync.Once
}

type memoryTicket struct {
	labels    map[string]string
	expiresAt time.Time
	size      int64
}

// Issue mints a 32-byte URL-safe random ticket, stores it +
// returns the encoded form. ttl <= 0 → 30s default (browser
// fetch → WS-connect round-trip is comfortably under).
//
// Returns ErrTicketStoreFull when storing the ticket would exceed the
// entry or byte limit even after expired tickets are swept.
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
	size := int64(len(ticket) + ticketEntryOverhead)
	var copied map[string]string
	if labels != nil {
		copied = make(map[string]string, len(labels))
		for k, v := range labels {
			copied[k] = v
			size += int64(len(k) + len(v))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.fitsLocked(size) {
		s.reapExpiredLocked(now)
		if !s.fitsLocked(size) {
			return "", ErrTicketStoreFull
		}
	}
	s.entries[ticket] = memoryTicket{labels: copied, expiresAt: now.Add(ttl), size: size}
	s.bytes += size
	return ticket, nil
}

// fitsLocked reports whether one more entry of size bytes stays within
// both limits. Caller holds s.mu.
func (s *MemoryTicketStore) fitsLocked(size int64) bool {
	return len(s.entries) < s.maxEntries && s.bytes+size <= s.maxBytes
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
	s.deleteLocked(ticket, entry)
	if s.now().After(entry.expiresAt) {
		return nil, ErrTicketInvalid
	}
	return entry.labels, nil
}

// deleteLocked removes one entry and releases its bytes. Caller holds s.mu.
func (s *MemoryTicketStore) deleteLocked(ticket string, e memoryTicket) {
	delete(s.entries, ticket)
	s.bytes -= e.size
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
	s.reapExpiredLocked(now)
}

// reapExpiredLocked drops every expired entry. Caller holds s.mu.
func (s *MemoryTicketStore) reapExpiredLocked(now time.Time) {
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			s.deleteLocked(k, e)
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
// 405 with Allow header). Principal failure → 401 (431 for
// ErrWSAuthTicketHeaderTooLarge); a full store
// (ErrTicketStoreFull) → 503 with Retry-After; other Store
// failures → 500.
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
			writeRefusalStatus(r.Context(), w, http.StatusMethodNotAllowed, codes.Unimplemented, "")
			return
		}
		// Every refusal below is the standard JSON error envelope, like
		// any other answer from this gateway, so a client that parses
		// errors one way keeps working on this route.
		labels, err := cfg.Principal(r)
		if err != nil {
			if errors.Is(err, ErrWSAuthTicketHeaderTooLarge) {
				writeRefusalStatus(r.Context(), w, http.StatusRequestHeaderFieldsTooLarge, codes.InvalidArgument, "ticket issuer: "+err.Error())
				return
			}
			WriteRefusal(r.Context(), w, codes.Unauthenticated, "ticket issuer: "+err.Error())
			return
		}
		ticket, err := cfg.Store.Issue(r.Context(), labels, cfg.TTL)
		if err != nil {
			if errors.Is(err, ErrTicketStoreFull) {
				w.Header().Set("Retry-After", "1")
				WriteRefusal(r.Context(), w, codes.Unavailable, "ticket issuer: "+err.Error())
				return
			}
			WriteInternal(r.Context(), w, "ticket issuer: "+err.Error())
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
// route tree.
func MountTicketIssuer(r chi.Router, path string, principal func(*http.Request) (map[string]string, error)) *MemoryTicketStore {
	store := NewMemoryTicketStore()
	r.Method(http.MethodPost, path, NewTicketIssuer(TicketIssuerConfig{
		Principal: principal,
		Store:     store,
	}))
	return store
}
