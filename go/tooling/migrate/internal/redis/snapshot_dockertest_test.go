//go:build dockertest

package redis_test

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
)

// TestSnapshotter_RoundTrip is the S2 redis verify: seed mixed-type
// keys → dump → flush → restore → values + TTL preserved. Gated by
// `//go:build dockertest`; run via:
//
//	go test -tags=dockertest ./domains/console/apply/redis/
func TestSnapshotter_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := startThrowawayRedis(ctx, t)
	client := goredis.NewClient(mustParse(t, dsn))
	defer client.Close()

	// Seed several value types + a TTL'd key.
	if err := client.Set(ctx, "str", "hello", 0).Err(); err != nil {
		t.Fatalf("seed str: %v", err)
	}
	if err := client.HSet(ctx, "hash", "f1", "v1", "f2", "v2").Err(); err != nil {
		t.Fatalf("seed hash: %v", err)
	}
	if err := client.RPush(ctx, "list", "a", "b", "c").Err(); err != nil {
		t.Fatalf("seed list: %v", err)
	}
	if err := client.Set(ctx, "ttlkey", "x", time.Hour).Err(); err != nil {
		t.Fatalf("seed ttlkey: %v", err)
	}

	snap, err := redis.NewSnapshotter(dsn)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}

	var dump bytes.Buffer
	if err := snap.Dump(ctx, &dump); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if dump.Len() == 0 {
		t.Fatal("Dump empty")
	}

	if err := client.FlushAll(ctx).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n, _ := client.DBSize(ctx).Result(); n != 0 {
		t.Fatalf("flush left %d keys", n)
	}

	if err := snap.Restore(ctx, bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got, _ := client.Get(ctx, "str").Result(); got != "hello" {
		t.Errorf("str = %q, want hello", got)
	}
	if got, _ := client.HGet(ctx, "hash", "f2").Result(); got != "v2" {
		t.Errorf("hash.f2 = %q, want v2", got)
	}
	if got, _ := client.LRange(ctx, "list", 0, -1).Result(); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("list = %v, want [a b c]", got)
	}
	ttl, _ := client.TTL(ctx, "ttlkey").Result()
	if ttl <= 0 || ttl > time.Hour {
		t.Errorf("ttlkey TTL = %v, want (0, 1h]", ttl)
	}
}

func mustParse(t *testing.T, dsn string) *goredis.Options {
	t.Helper()
	opts, err := goredis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return opts
}

func startThrowawayRedis(ctx context.Context, t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "-P", "redis:7-alpine").Output()
	if err != nil {
		t.Skipf("docker run failed (no docker?): %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "stop", id).Run() })

	portOut, err := exec.CommandContext(ctx, "docker", "port", id, "6379/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	first := strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0]
	port := first[strings.LastIndex(first, ":")+1:]
	dsn := fmt.Sprintf("redis://127.0.0.1:%s/0", port)

	deadline := time.Now().Add(60 * time.Second)
	for {
		c := goredis.NewClient(mustParse(t, dsn))
		err := c.Ping(ctx).Err()
		_ = c.Close()
		if err == nil {
			return dsn
		}
		if time.Now().After(deadline) {
			t.Fatalf("redis not ready: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
