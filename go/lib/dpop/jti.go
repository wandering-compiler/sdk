package dpop

import (
	"context"
	"errors"
	"sync"
	"time"
)

// JTIRecord is what a [JTIStore] keeps per accepted proof.
type JTIRecord struct {
	// ProofHash is the SHA-256 of the whole proof, hex. A presentation with
	// the same jti and the same hash is the same proof presented again.
	ProofHash string
	// FirstSeen is when the proof was first accepted.
	FirstSeen time.Time
}

// JTIStore remembers the jti of every accepted proof for as long as the proof
// could still be accepted.
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

// ErrJTIStoreFull is returned by [MemoryJTIStore.Record] when the store is at
// its limit and no entry has expired. The verifier fails closed: the request
// is refused, never accepted unrecorded.
var ErrJTIStoreFull = errors.New("dpop: jti store full")

// DefaultMaxJTIs bounds the entries of a [MemoryJTIStore].
const DefaultMaxJTIs = 200_000

// jtiSweepEvery is how often (in store time) Record sweeps expired entries
// even when the store is not full.
const jtiSweepEvery = time.Minute

type jtiEntry struct {
	rec     JTIRecord
	expires time.Time
}

// MemoryJTIStore is an in-process [JTIStore] for a single replica.
type MemoryJTIStore struct {
	mu        sync.Mutex
	m         map[string]jtiEntry
	max       int
	nextSweep time.Time
}

// NewMemoryJTIStore returns an empty store holding at most maxEntries jtis;
// maxEntries <= 0 means [DefaultMaxJTIs].
func NewMemoryJTIStore(maxEntries int) *MemoryJTIStore {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxJTIs
	}
	return &MemoryJTIStore{m: make(map[string]jtiEntry), max: maxEntries}
}

// Record implements [JTIStore]. The time it judges expiry by is
// rec.FirstSeen, the verifier's clock.
func (s *MemoryJTIStore) Record(_ context.Context, key string, rec JTIRecord, ttl time.Duration) (JTIRecord, bool, error) {
	now := rec.FirstSeen
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[key]; ok {
		if now.Before(e.expires) {
			return e.rec, false, nil
		}
		delete(s.m, key)
	}
	if !now.Before(s.nextSweep) {
		s.sweep(now)
	}
	if len(s.m) >= s.max {
		s.sweep(now)
		if len(s.m) >= s.max {
			return JTIRecord{}, false, ErrJTIStoreFull
		}
	}
	s.m[key] = jtiEntry{rec: rec, expires: now.Add(ttl)}
	return rec, true, nil
}

func (s *MemoryJTIStore) sweep(now time.Time) {
	for k, e := range s.m {
		if !now.Before(e.expires) {
			delete(s.m, k)
		}
	}
	s.nextSweep = now.Add(jtiSweepEvery)
}

// Len reports the number of stored entries, expired ones included until the
// next sweep.
func (s *MemoryJTIStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
