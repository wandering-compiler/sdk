// Short-lived one-shot ticket store for browser auth on
// streaming endpoints.
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
	"container/heap"
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

	"github.com/wandering-compiler/sdk/go/lib/ratelimit"
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

// ErrTicketAddressFull is returned by Issue when the client address the
// ticket is asked for (ClientAddrFrom of Issue's context) already holds its
// share of outstanding tickets. The issuer answers 429 with Retry-After.
var ErrTicketAddressFull = errors.New("restgw: too many outstanding tickets for this address")

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
	// DefaultMaxTicketsPerAddress bounds the outstanding tickets of one
	// client address, so no single address can fill the store and starve
	// every other browser on the replica. A browser redeems a ticket within
	// moments of getting it, so a legitimate address rarely holds more than
	// a handful; the cap leaves room for many clients behind one NAT.
	DefaultMaxTicketsPerAddress = 64
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

// WithMaxTicketsPerAddress sets how many outstanding tickets one client
// address may hold. n <= 0 keeps the default.
func WithMaxTicketsPerAddress(n int) MemoryTicketStoreOption {
	return func(s *MemoryTicketStore) {
		if n > 0 {
			s.maxPerAddr = n
		}
	}
}

// NewMemoryTicketStore builds the in-process default. Background
// janitor evicts expired entries every minute; Issue also sweeps
// expired entries itself when the store is at a limit. Call Close on
// shutdown to stop the janitor (optional — usable without).
//
// Limits: at most DefaultMaxTickets outstanding tickets holding at most
// DefaultMaxTicketBytes (override with WithMaxTickets / WithMaxTicketBytes),
// and at most DefaultMaxTicketsPerAddress of them for one client address
// (WithMaxTicketsPerAddress). The address is ClientAddrFrom of Issue's
// context — NewTicketIssuer always puts one there — and a ticket stops
// counting against it once it is redeemed or expired. Issue with no address
// in its context (a programmatic caller) is bounded by the store limits only.
// When full, Issue refuses with ErrTicketStoreFull rather than evicting:
// evicting would let new requests cancel tickets already handed to other
// clients, while refusing only delays new ones until the TTL frees room.
func NewMemoryTicketStore(opts ...MemoryTicketStoreOption) *MemoryTicketStore {
	s := &MemoryTicketStore{
		entries:    make(map[string]*memoryTicket),
		byAddr:     make(map[string][]string),
		stop:       make(chan struct{}),
		maxEntries: DefaultMaxTickets,
		maxBytes:   DefaultMaxTicketBytes,
		maxPerAddr: DefaultMaxTicketsPerAddress,
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
	mu      sync.Mutex
	entries map[string]*memoryTicket
	// expiry orders the entries by expiresAt (a min-heap), so a sweep
	// removes what expired without walking the live tickets.
	expiry     ticketExpiry
	bytes      int64
	maxEntries int
	maxBytes   int64
	// byAddr lists the outstanding tickets of each client address, at most
	// maxPerAddr long; an address with none has no key.
	byAddr     map[string][]string
	maxPerAddr int
	now        func() time.Time
	stop       chan struct{}
	stopOnce   sync.Once
}

type memoryTicket struct {
	ticket    string
	labels    map[string]string
	expiresAt time.Time
	size      int64
	addr      string
	index     int // position in MemoryTicketStore.expiry
}

// ticketExpiry is a min-heap of the outstanding tickets by expiry
// (container/heap); each entry knows its index so a redeemed ticket is
// removed from it in O(log n).
type ticketExpiry []*memoryTicket

func (h ticketExpiry) Len() int           { return len(h) }
func (h ticketExpiry) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h ticketExpiry) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *ticketExpiry) Push(x any) {
	e := x.(*memoryTicket)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *ticketExpiry) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return e
}

// Issue mints a 32-byte URL-safe random ticket, stores it +
// returns the encoded form. ttl <= 0 → 30s default (browser
// fetch → WS-connect round-trip is comfortably under).
//
// Returns ErrTicketStoreFull when storing the ticket would exceed the
// entry or byte limit even after expired tickets are swept, and
// ErrTicketAddressFull when the client address in ctx (ClientAddrFrom)
// already holds its cap of unexpired tickets.
func (s *MemoryTicketStore) Issue(ctx context.Context, labels map[string]string, ttl time.Duration) (string, error) {
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
	addr := ClientAddrFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if addr != "" && len(s.byAddr[addr]) >= s.maxPerAddr {
		// Only this address's own tickets are looked at: a flooding
		// address costs O(its cap) per refusal, never a sweep of the store.
		s.reapAddrLocked(addr, now)
		if len(s.byAddr[addr]) >= s.maxPerAddr {
			return "", ErrTicketAddressFull
		}
	}
	if !s.fitsLocked(size) {
		s.reapExpiredLocked(now)
		if !s.fitsLocked(size) {
			return "", ErrTicketStoreFull
		}
	}
	e := &memoryTicket{ticket: ticket, labels: copied, expiresAt: now.Add(ttl), size: size, addr: addr}
	s.entries[ticket] = e
	heap.Push(&s.expiry, e)
	s.bytes += size
	if addr != "" {
		s.byAddr[addr] = append(s.byAddr[addr], ticket)
	}
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
	s.deleteLocked(entry)
	if s.now().After(entry.expiresAt) {
		return nil, ErrTicketInvalid
	}
	return entry.labels, nil
}

// deleteLocked removes one entry and releases its bytes and its address's
// slot. Caller holds s.mu.
func (s *MemoryTicketStore) deleteLocked(e *memoryTicket) {
	ticket := e.ticket
	delete(s.entries, ticket)
	heap.Remove(&s.expiry, e.index)
	s.bytes -= e.size
	if e.addr == "" {
		return
	}
	list := s.byAddr[e.addr]
	for i, t := range list {
		if t == ticket {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(s.byAddr, e.addr)
	} else {
		s.byAddr[e.addr] = list
	}
}

// reapAddrLocked drops the expired tickets of one address. Caller holds s.mu.
func (s *MemoryTicketStore) reapAddrLocked(addr string, now time.Time) {
	for _, t := range append([]string(nil), s.byAddr[addr]...) {
		if e, ok := s.entries[t]; ok && now.After(e.expiresAt) {
			s.deleteLocked(e)
		}
	}
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

// reapExpiredLocked drops every expired entry, earliest first, and stops at
// the first live one: its cost is what it removes, so a full store of live
// tickets refuses in O(1). Caller holds s.mu.
func (s *MemoryTicketStore) reapExpiredLocked(now time.Time) {
	for len(s.expiry) > 0 && now.After(s.expiry[0].expiresAt) {
		s.deleteLocked(s.expiry[0])
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
// ErrWSAuthTicketHeaderTooLarge); an address at its cap
// (ErrTicketAddressFull) → 429 with Retry-After; a full store
// (ErrTicketStoreFull) → 503 with Retry-After; other Store
// failures → 500.
//
// The store is asked with the caller's address in the context
// ([ClientAddrFrom]): the one [ClientAddrMiddleware] resolved behind the
// trusted proxies when it runs in front, else the connection's peer. The
// issuer mints before the credential is checked (redeem checks it), so this
// per-address share is what keeps one unauthenticated client from filling
// the store and denying tickets to every other browser.
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
		if ClientAddrFrom(r.Context()) == "" {
			r = r.WithContext(context.WithValue(r.Context(), clientAddrCtxKey{}, ratelimit.HTTPClientAddr(r, nil)))
		}
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
			if errors.Is(err, ErrTicketAddressFull) {
				w.Header().Set("Retry-After", "1")
				WriteRefusal(r.Context(), w, codes.ResourceExhausted, "ticket issuer: "+err.Error())
				return
			}
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
