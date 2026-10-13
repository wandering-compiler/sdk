package dpopstore_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopstore"
)

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func redisHarness(client *redis.Client, advance func(t *testing.T, d time.Duration)) harness {
	return harness{
		newStore: func(t *testing.T, maxPerKey int) dpop.JTIStore {
			s, err := dpopstore.NewRedisJTIStore(client,
				dpopstore.WithRedisPrefix("w17:test:dpop:"+randomSuffix(t)+":"),
				dpopstore.WithRedisMaxPerKey(maxPerKey))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		advance: advance,
	}
}

func TestRedisJTIStore_Contract(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	runContract(t, redisHarness(client, func(_ *testing.T, d time.Duration) { mr.FastForward(d) }))
}

// The jti key carries its ttl, so Redis forgets it on its own; nothing
// outlives the replay window.
func TestRedisJTIStore_KeysCarryTheTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	s, err := dpopstore.NewRedisJTIStore(client)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Record(context.Background(), "tp\x00j", dpop.JTIRecord{Thumbprint: "tp", ProofHash: "h", FirstSeen: time.Now()}, time.Minute); err != nil {
		t.Fatal(err)
	}
	keys := mr.Keys()
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want the jti key and the per-key set", keys)
	}
	for _, k := range keys {
		if ttl := mr.TTL(k); ttl <= 0 || ttl > time.Minute {
			t.Errorf("%s: ttl %v, want (0, 1m]", k, ttl)
		}
	}
	mr.FastForward(time.Minute + time.Second)
	if keys := mr.Keys(); len(keys) != 0 {
		t.Errorf("keys outlived the ttl: %v", keys)
	}
}

// A Redis that does not answer fails the Record; the verifier then refuses
// with ErrUnavailable instead of accepting an unrecorded proof.
func TestRedisJTIStore_FailingBackendFailsClosed(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	s, err := dpopstore.NewRedisJTIStore(client)
	if err != nil {
		t.Fatal(err)
	}
	mr.Close()
	_, created, err := s.Record(context.Background(), "tp\x00j", dpop.JTIRecord{Thumbprint: "tp", ProofHash: "h", FirstSeen: time.Now()}, time.Minute)
	if err == nil || created {
		t.Fatalf("Record against a dead Redis = (%v, %v), want an error", created, err)
	}
	if errors.Is(err, dpop.ErrJTIStoreFull) {
		t.Error("an outage reported as a full store")
	}
	if _, err := dpopstore.NewRedisJTIStore(nil); err == nil {
		t.Error("a store without a client was built")
	}
}

// The same contract against a real Redis (W17_REDIS_ADDR); CI sets
// W17_REQUIRE_LIVE_REDIS=1 so it cannot skip.
func TestRedisJTIStore_Contract_Live(t *testing.T) {
	addr := os.Getenv("W17_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("W17_REQUIRE_LIVE_REDIS") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_REDIS=1 but W17_REDIS_ADDR is not set — refusing to skip-green")
		}
		t.Skip("W17_REDIS_ADDR not set — live Redis test skipped")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	runContract(t, redisHarness(client, func(_ *testing.T, d time.Duration) { time.Sleep(d) }))
}
