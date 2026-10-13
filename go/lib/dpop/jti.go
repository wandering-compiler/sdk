package dpop

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"
)

// JTIRecord is what a [JTIStore] keeps per accepted proof.
type JTIRecord struct {
	// Thumbprint is the key that signed the proof, for per-key limits.
	Thumbprint string
	// ProofHash is the SHA-256 of the whole proof, hex. A presentation with
	// the same jti and the same hash is the same proof presented again.
	ProofHash string
	// FirstSeen is when the proof was first accepted.
	FirstSeen time.Time
}

// JTIStore remembers the jti of every accepted proof for as long as the proof
// could still be accepted. The verifier calls it only for a proof that passed
// every other check.
//
// Record must be atomic: if key is absent (or its entry has expired at
// rec.FirstSeen), it stores rec for ttl and returns (rec, true); otherwise it
// returns the stored record and false. The verifier decides what a repeat
// means.
//
// The in-memory [MemoryJTIStore] is per process and therefore correct only
// with a single replica. With several replicas a proof accepted by one could
// be replayed against another; they need a shared implementation (Redis, a
// database) behind this interface.
type JTIStore interface {
	Record(ctx context.Context, key string, rec JTIRecord, ttl time.Duration) (JTIRecord, bool, error)
}

// ErrJTIStoreFull is returned by [MemoryJTIStore.Record] when the store, or
// the share of it one key may hold, is at its limit and no entry has
// expired. The verifier fails closed: the request is refused, never accepted
// unrecorded.
var ErrJTIStoreFull = errors.New("dpop: jti store full")

// Default limits of a [MemoryJTIStore].
const (
	// DefaultMaxJTIs bounds the entries of the whole store.
	DefaultMaxJTIs = 200_000
	// DefaultMaxJTIsPerKey bounds the entries one key thumbprint may hold,
	// so a single bound key cannot take the store from everyone else.
	DefaultMaxJTIsPerKey = 10_000
)

type jtiEntry struct {
	rec     JTIRecord
	expires time.Time
}

// expiry is one heap item: entries leave the store in expiry order, so
// freeing room never scans the store.
type expiry struct {
	key     string
	expires time.Time
}

type expiryHeap []expiry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *expiryHeap) Push(x any)        { *h = append(*h, x.(expiry)) }
func (h *expiryHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// MemoryJTIStore is an in-process [JTIStore] for a single replica. Expired
// entries are dropped in expiry order (a min-heap), at a cost of O(log n)
// each, as later Records pass their expiry time.
type MemoryJTIStore struct {
	mu     sync.Mutex
	m      map[string]jtiEntry
	byKey  map[string]int // thumbprint → live entries
	exp    expiryHeap
	max    int
	maxKey int
}

// MemoryJTIOption configures [NewMemoryJTIStore].
type MemoryJTIOption func(*MemoryJTIStore)

// WithMaxJTIsPerKey bounds the entries one key thumbprint may hold; n <= 0
// keeps [DefaultMaxJTIsPerKey].
func WithMaxJTIsPerKey(n int) MemoryJTIOption {
	return func(s *MemoryJTIStore) {
		if n > 0 {
			s.maxKey = n
		}
	}
}

// NewMemoryJTIStore returns an empty store holding at most maxEntries jtis;
// maxEntries <= 0 means [DefaultMaxJTIs].
func NewMemoryJTIStore(maxEntries int, opts ...MemoryJTIOption) *MemoryJTIStore {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxJTIs
	}
	s := &MemoryJTIStore{
		m:      make(map[string]jtiEntry),
		byKey:  make(map[string]int),
		max:    maxEntries,
		maxKey: DefaultMaxJTIsPerKey,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Record implements [JTIStore]. The time it judges expiry by is
// rec.FirstSeen, the verifier's clock.
func (s *MemoryJTIStore) Record(_ context.Context, key string, rec JTIRecord, ttl time.Duration) (JTIRecord, bool, error) {
	now := rec.FirstSeen
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
	if e, ok := s.m[key]; ok {
		return e.rec, false, nil
	}
	if len(s.m) >= s.max || s.byKey[rec.Thumbprint] >= s.maxKey {
		return JTIRecord{}, false, ErrJTIStoreFull
	}
	e := jtiEntry{rec: rec, expires: now.Add(ttl)}
	s.m[key] = e
	s.byKey[rec.Thumbprint]++
	heap.Push(&s.exp, expiry{key: key, expires: e.expires})
	return rec, true, nil
}

// expire drops every entry whose expiry is not after now.
func (s *MemoryJTIStore) expire(now time.Time) {
	for len(s.exp) > 0 && !now.Before(s.exp[0].expires) {
		x := heap.Pop(&s.exp).(expiry)
		e, ok := s.m[x.key]
		if !ok || !e.expires.Equal(x.expires) {
			continue
		}
		delete(s.m, x.key)
		tp := e.rec.Thumbprint
		if s.byKey[tp]--; s.byKey[tp] <= 0 {
			delete(s.byKey, tp)
		}
	}
}

// Len reports the number of stored entries, expired ones included until the
// next Record passes their expiry.
func (s *MemoryJTIStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
