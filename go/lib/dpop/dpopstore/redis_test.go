package dpopstore_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
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
		newStore: func(t *testing.T, maxPerKey, maxTotal int) dpop.JTIStore {
			s, err := dpopstore.NewRedisJTIStore(client,
				dpopstore.WithRedisPrefix("w17:test:dpop:"+randomSuffix(t)+":"),
				dpopstore.WithRedisMaxPerKey(maxPerKey),
				dpopstore.WithRedisMaxEntries(maxTotal))
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
	if len(keys) != 3 {
		t.Fatalf("keys = %v, want the jti key, the per-key set and the total set", keys)
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

// A flood of unbound proofs from throwaway keys, each a new thumbprint, takes
// at most the mint store's total cap of Redis — refused proofs leave no key
// behind — so the memory a noeviction Redis has left for the bound store is
// not consumed, and the bound store keeps recording.
func TestRedisJTIStore_FloodStaysWithinTheCap(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	const mintCap = 50
	mint, err := dpopstore.NewRedisJTIStore(client, dpopstore.WithRedisPrefix("w17:mint:"), dpopstore.WithRedisMaxEntries(mintCap))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := dpopstore.NewRedisJTIStore(client, dpopstore.WithRedisPrefix("w17:bound:"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	full := 0
	for i := range 10 * mintCap {
		tp := fmt.Sprint("throwaway-", i)
		_, _, err := mint.Record(ctx, tp+"\x00j", dpop.JTIRecord{Thumbprint: tp, ProofHash: "h", FirstSeen: now}, time.Minute)
		switch {
		case errors.Is(err, dpop.ErrJTIStoreFull):
			full++
		case err != nil:
			t.Fatal(err)
		}
	}
	if full != 9*mintCap {
		t.Fatalf("%d of %d flood records refused, want %d", full, 10*mintCap, 9*mintCap)
	}
	mintKeys := 0
	for _, k := range mr.Keys() {
		if strings.HasPrefix(k, "w17:mint:") {
			mintKeys++
		}
	}
	// One jti key and one per-key set per recorded proof, plus the total set.
	if mintKeys > 2*mintCap+1 {
		t.Fatalf("the flood left %d mint keys, want at most %d", mintKeys, 2*mintCap+1)
	}
	for i := range 3 {
		if _, created, err := bound.Record(ctx, fmt.Sprint("dev\x00", i), dpop.JTIRecord{Thumbprint: "dev", ProofHash: fmt.Sprint(i), FirstSeen: now}, time.Minute); err != nil || !created {
			t.Fatalf("bound record %d after the flood: %v %v", i, created, err)
		}
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
