package kvposture

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// liveRedis connects to the Redis W17_REDIS_ADDR names. W17_REQUIRE_LIVE_REDIS=1
// turns a missing address into a failure, so CI cannot go green by skipping.
func liveRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("W17_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("W17_REQUIRE_LIVE_REDIS") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_REDIS=1 but W17_REDIS_ADDR is not set — refusing to skip-green")
		}
		t.Skip("W17_REDIS_ADDR not set — live Redis test skipped")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// Against a real server the check reads the policy it is running with: an
// `allkeys-*` policy warns at boot, naming the connection and the policy;
// `noeviction` and `volatile-*` (which never evict a key without a TTL, and
// stored entities carry none) say nothing.
func TestWarnIfEvicting_ReadsTheServersPolicy_Live(t *testing.T) {
	client := liveRedis(t)
	ctx := context.Background()
	orig, err := client.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil {
		t.Fatalf("CONFIG GET: %v", err)
	}
	t.Cleanup(func() { _ = client.ConfigSet(context.Background(), "maxmemory-policy", orig["maxmemory-policy"]).Err() })

	for policy, wantWarn := range map[string]bool{
		"allkeys-lru":  true,
		"allkeys-lfu":  true,
		"noeviction":   false,
		"volatile-lru": false,
	} {
		t.Run(policy, func(t *testing.T) {
			if err := client.ConfigSet(ctx, "maxmemory-policy", policy).Err(); err != nil {
				t.Fatalf("CONFIG SET: %v", err)
			}
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			WarnIfEvicting(ctx, "catalog-redis", client)
			slog.SetDefault(prev)

			out := buf.String()
			warned := strings.Contains(out, "level=WARN")
			if warned != wantWarn {
				t.Fatalf("policy %s: warned = %v, want %v (log %q)", policy, warned, wantWarn, out)
			}
			if wantWarn && (!strings.Contains(out, "catalog-redis") || !strings.Contains(out, policy)) {
				t.Errorf("warning does not name the connection and the policy: %q", out)
			}
		})
	}
}
