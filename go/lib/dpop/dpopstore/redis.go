// Package dpopstore holds shared [dpop.JTIStore] implementations for
// deployments with more than one replica: one over Redis ([RedisJTIStore])
// and one over a SQL database ([SQLJTIStore]). With a store shared by every
// replica, a proof accepted by one replica is a replay on all of them, which
// the per-process [dpop.MemoryJTIStore] cannot promise.
//
// Both keep the [dpop.JTIStore] contract: Record is an atomic
// insert-if-absent that returns the stored record when the key is already
// there, entries expire after their ttl as judged by rec.FirstSeen (the
// verifier's clock), one key thumbprint may hold only its share of the
// store, and the store as a whole only its total cap ([dpop.ErrJTIStoreFull]
// beyond either). A failing backend returns its
// error; the verifier then refuses the request with dpop.ErrUnavailable (503),
// never accepting a proof it could not record.
//
// Configure one store for bound proofs and a separate one (another key prefix
// or table) for unbound mint proofs, as [dpop.Config.MintJTIs] requires. The
// total cap is per store, so a flood of unbound proofs from throwaway keys
// fills only the mint store; give that one a smaller cap
// ([dpop.DefaultMaxMintJTIs] is the in-memory default).
package dpopstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

// DefaultRedisPrefix is the key prefix of a [RedisJTIStore].
const DefaultRedisPrefix = "w17:dpop:jti:"

// RedisJTIStore is a [dpop.JTIStore] in Redis, shared by every replica that
// talks to the same Redis. Each accepted proof is one key with a TTL; one
// sorted set per thumbprint counts the key's live entries for the per-key
// share, and one sorted set per store (per prefix) counts every live entry
// for the total cap. Both keys of a thumbprint share a hash tag, so the
// store works on a Redis Cluster.
//
// Record runs two Lua scripts, each atomic: the first reserves a place under
// the total cap (or refuses with [dpop.ErrJTIStoreFull]), the second records
// the jti under the per-key share. A reservation the second script does not
// use — the key's share is full, the jti was already recorded, or the script
// failed — is released; a full total set is checked against the jti key
// itself, so a recorded jti is answered as a repeat even when full; one
// left behind by a crash expires with the entry's ttl. The total set lives
// in one hash slot of its own, apart from the jti keys, which is why the
// two steps are separate scripts.
//
// Expiry runs on the Redis server's clock (each jti key carries its ttl);
// the per-key count and the total are judged by rec.FirstSeen. Run it on a
// Redis whose maxmemory-policy is noeviction, since an evicted jti is a
// replay window reopened early; the total caps of the stores sharing that
// Redis bound what they can take of its memory, so a flood that fills the
// mint store leaves the memory the bound store needs.
type RedisJTIStore struct {
	rdb      redis.Scripter
	prefix   string
	maxKey   int
	maxTotal int
}

// RedisOption configures [NewRedisJTIStore].
type RedisOption func(*RedisJTIStore)

// WithRedisPrefix sets the key prefix (default [DefaultRedisPrefix]). Use a
// different prefix for the mint store.
func WithRedisPrefix(p string) RedisOption {
	return func(s *RedisJTIStore) {
		if p != "" {
			s.prefix = p
		}
	}
}

// WithRedisMaxPerKey bounds the live entries one key thumbprint may hold
// (default [dpop.DefaultMaxJTIsPerKey]); n <= 0 keeps the default.
func WithRedisMaxPerKey(n int) RedisOption {
	return func(s *RedisJTIStore) {
		if n > 0 {
			s.maxKey = n
		}
	}
}

// WithRedisMaxEntries bounds the live entries of the whole store (default
// [dpop.DefaultMaxJTIs]); n <= 0 keeps the default. Size it for the peak
// rate of proofs times the jti ttl (the nonce lifetime, or twice the iat
// window); a full store refuses with [dpop.ErrJTIStoreFull].
func WithRedisMaxEntries(n int) RedisOption {
	return func(s *RedisJTIStore) {
		if n > 0 {
			s.maxTotal = n
		}
	}
}

// NewRedisJTIStore returns a store over rdb (a *redis.Client,
// *redis.ClusterClient or any redis.UniversalClient).
func NewRedisJTIStore(rdb redis.Scripter, opts ...RedisOption) (*RedisJTIStore, error) {
	if rdb == nil {
		return nil, errors.New("dpopstore: NewRedisJTIStore needs a client")
	}
	s := &RedisJTIStore{rdb: rdb, prefix: DefaultRedisPrefix, maxKey: dpop.DefaultMaxJTIsPerKey, maxTotal: dpop.DefaultMaxJTIs}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// reserveScript: KEYS[1] the store's total set. ARGV: the jti key (the
// member), now ms, max entries, expiry ms, ttl ms. Returns 1 reserved, 0
// already counted (the jti is recorded or being recorded), 2 the store is
// full.
var reserveScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[2])
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then return 0 end
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[3]) then return 2 end
redis.call('ZADD', KEYS[1], ARGV[4], ARGV[1])
if redis.call('PTTL', KEYS[1]) < tonumber(ARGV[5]) then
  redis.call('PEXPIRE', KEYS[1], ARGV[5])
end
return 1
`)

// storedScript: KEYS[1] the jti key. Returns its stored record, or nil.
var storedScript = redis.NewScript(`return redis.call('GET', KEYS[1])`)

// releaseScript: KEYS[1] the store's total set, ARGV[1] the jti key.
var releaseScript = redis.NewScript(`return redis.call('ZREM', KEYS[1], ARGV[1])`)

// recordScript: KEYS[1] the jti key, KEYS[2] the thumbprint's counter set.
// ARGV: record JSON, ttl ms, now ms, max per key, expiry ms.
// Returns {1} stored, {0, json} already there, {2} the key's share is full.
var recordScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v then return {0, v} end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[3])
if redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[4]) then return {2} end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
if redis.call('PTTL', KEYS[2]) < tonumber(ARGV[2]) then
  redis.call('PEXPIRE', KEYS[2], ARGV[2])
end
return {1}
`)

// redisRecord is the stored form of a [dpop.JTIRecord].
type redisRecord struct {
	Thumbprint string `json:"t"`
	ProofHash  string `json:"h"`
	FirstSeen  int64  `json:"f"` // unix nanoseconds
}

// Record implements [dpop.JTIStore].
func (s *RedisJTIStore) Record(ctx context.Context, key string, rec dpop.JTIRecord, ttl time.Duration) (dpop.JTIRecord, bool, error) {
	if ttl < time.Millisecond {
		ttl = time.Millisecond
	}
	body, err := json.Marshal(redisRecord{Thumbprint: rec.Thumbprint, ProofHash: rec.ProofHash, FirstSeen: rec.FirstSeen.UnixNano()})
	if err != nil {
		return dpop.JTIRecord{}, false, err
	}
	sum := sha256.Sum256([]byte(key))
	tag := "{" + hashTag(rec.Thumbprint) + "}"
	jtiKey := s.prefix + tag + ":" + hex.EncodeToString(sum[:])
	setKey := s.prefix + tag + ":n"
	totalKey := s.prefix + "{all}:n"
	now := rec.FirstSeen.UnixMilli()
	exp := now + ttl.Milliseconds()

	reserved, err := reserveScript.Run(ctx, s.rdb, []string{totalKey},
		jtiKey, now, s.maxTotal, exp, ttl.Milliseconds()).Int64()
	switch {
	case err != nil:
		return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: redis reserve: %w", err)
	case reserved == 2:
		// The total set and the jti key live in different hash slots, so
		// the reservation could not see the jti itself. A jti can be
		// recorded while its member is gone (the record landed but the
		// client saw an error and released), so a full store still answers
		// for what it holds: a stored jti is a repeat, never "full".
		raw, err := storedScript.Run(ctx, s.rdb, []string{jtiKey}).Text()
		switch {
		case errors.Is(err, redis.Nil):
			return dpop.JTIRecord{}, false, dpop.ErrJTIStoreFull
		case err != nil:
			return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: redis read: %w", err)
		}
		return decodeStored(raw)
	}
	// release gives back a reservation the record below did not use. A
	// failed release leaves the place taken until the entry's ttl.
	release := func() {
		if reserved == 1 {
			_ = releaseScript.Run(context.WithoutCancel(ctx), s.rdb, []string{totalKey}, jtiKey).Err()
		}
	}

	res, err := recordScript.Run(ctx, s.rdb, []string{jtiKey, setKey},
		string(body), ttl.Milliseconds(), now, s.maxKey, exp).Slice()
	if err != nil {
		release()
		return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: redis record: %w", err)
	}
	if len(res) == 0 {
		release()
		return dpop.JTIRecord{}, false, errors.New("dpopstore: redis record: empty reply")
	}
	switch code, _ := res[0].(int64); code {
	case 1:
		return rec, true, nil
	case 2:
		release()
		return dpop.JTIRecord{}, false, dpop.ErrJTIStoreFull
	case 0:
		// Already recorded: the place reserved for it is not needed.
		release()
		if len(res) < 2 {
			return dpop.JTIRecord{}, false, errors.New("dpopstore: redis record: no stored record")
		}
		raw, _ := res[1].(string)
		return decodeStored(raw)
	}
	release()
	return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: redis record: unexpected reply %v", res)
}

// decodeStored reads a stored record: the repeat answer of Record.
func decodeStored(raw string) (dpop.JTIRecord, bool, error) {
	var got redisRecord
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: redis record: stored value: %w", err)
	}
	return dpop.JTIRecord{Thumbprint: got.Thumbprint, ProofHash: got.ProofHash, FirstSeen: time.Unix(0, got.FirstSeen)}, false, nil
}

// hashTag is the cluster hash tag of a thumbprint: its SHA-256, hex, so it
// holds no brace and has a fixed length whatever the thumbprint is.
func hashTag(thumbprint string) string {
	sum := sha256.Sum256([]byte(thumbprint))
	return hex.EncodeToString(sum[:16])
}
