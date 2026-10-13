// Package dpopstore holds shared [dpop.JTIStore] implementations for
// deployments with more than one replica: one over Redis ([RedisJTIStore])
// and one over a SQL database ([SQLJTIStore]). With a store shared by every
// replica, a proof accepted by one replica is a replay on all of them, which
// the per-process [dpop.MemoryJTIStore] cannot promise.
//
// Both keep the [dpop.JTIStore] contract: Record is an atomic
// insert-if-absent that returns the stored record when the key is already
// there, entries expire after their ttl as judged by rec.FirstSeen (the
// verifier's clock), and one key thumbprint may hold only its share of the
// store ([dpop.ErrJTIStoreFull] beyond it). A failing backend returns its
// error; the verifier then refuses the request with dpop.ErrUnavailable (503),
// never accepting a proof it could not record.
//
// Configure one store for bound proofs and a separate one (another key prefix
// or table) for unbound mint proofs, as [dpop.Config.MintJTIs] requires.
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
// share. Both keys of a thumbprint share a hash tag, so the store works on a
// Redis Cluster, and Record runs as one Lua script, so it is atomic.
//
// Expiry runs on the Redis server's clock (each jti key carries its ttl);
// the per-key count is judged by rec.FirstSeen. The total size is bounded by
// Redis memory, not by the store: run it on a Redis whose maxmemory-policy
// is noeviction, since an evicted jti is a replay window reopened early.
type RedisJTIStore struct {
	rdb    redis.Scripter
	prefix string
	maxKey int
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

// NewRedisJTIStore returns a store over rdb (a *redis.Client,
// *redis.ClusterClient or any redis.UniversalClient).
func NewRedisJTIStore(rdb redis.Scripter, opts ...RedisOption) (*RedisJTIStore, error) {
	if rdb == nil {
		return nil, errors.New("dpopstore: NewRedisJTIStore needs a client")
	}
	s := &RedisJTIStore{rdb: rdb, prefix: DefaultRedisPrefix, maxKey: dpop.DefaultMaxJTIsPerKey}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

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
	now := rec.FirstSeen.UnixMilli()
	res, err := recordScript.Run(ctx, s.rdb, []string{jtiKey, setKey},
		string(body), ttl.Milliseconds(), now, s.maxKey, now+ttl.Milliseconds()).Slice()
	if err != nil {
		return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: redis record: %w", err)
	}
	if len(res) == 0 {
		return dpop.JTIRecord{}, false, errors.New("dpopstore: redis record: empty reply")
	}
	switch code, _ := res[0].(int64); code {
	case 1:
		return rec, true, nil
	case 2:
		return dpop.JTIRecord{}, false, dpop.ErrJTIStoreFull
	case 0:
		if len(res) < 2 {
			return dpop.JTIRecord{}, false, errors.New("dpopstore: redis record: no stored record")
		}
		raw, _ := res[1].(string)
		var got redisRecord
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: redis record: stored value: %w", err)
		}
		return dpop.JTIRecord{Thumbprint: got.Thumbprint, ProofHash: got.ProofHash, FirstSeen: time.Unix(0, got.FirstSeen)}, false, nil
	}
	return dpop.JTIRecord{}, false, fmt.Errorf("dpopstore: redis record: unexpected reply %v", res)
}

// hashTag is the cluster hash tag of a thumbprint: its SHA-256, hex, so it
// holds no brace and has a fixed length whatever the thumbprint is.
func hashTag(thumbprint string) string {
	sum := sha256.Sum256([]byte(thumbprint))
	return hex.EncodeToString(sum[:16])
}
