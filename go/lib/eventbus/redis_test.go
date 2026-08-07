package eventbus

// White-box unit tests for the Redis Streams adapter's
// validation + pure-function helpers. Live-broker integration
// deferred to a follow-up slice via the existing
// the container-based integration pattern.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewRedisBus_EmptyDSN(t *testing.T) {
	_, err := NewRedisBus(RedisBusOptions{
		GroupPrefix: "users-subscribers",
	})
	if err == nil || !strings.Contains(err.Error(), "DSN is empty") {
		t.Errorf("expected DSN-empty error, got %v", err)
	}
}

func TestNewRedisBus_EmptyGroupPrefix(t *testing.T) {
	_, err := NewRedisBus(RedisBusOptions{
		DSN: "redis://localhost:6379",
	})
	if err == nil || !strings.Contains(err.Error(), "GroupPrefix is empty") {
		t.Errorf("expected GroupPrefix-empty error, got %v", err)
	}
}

func TestNewRedisBus_BadDSN(t *testing.T) {
	_, err := NewRedisBus(RedisBusOptions{
		DSN:         "not-a-valid-dsn",
		GroupPrefix: "users",
	})
	if err == nil {
		t.Fatal("expected DSN parse error")
	}
	// go-redis returns "invalid URL scheme" or similar.
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("error should reference DSN: %v", err)
	}
}

func TestNewRedisBus_DefaultsApplied(t *testing.T) {
	// Construct with explicit ConsumerName so the hostname
	// lookup doesn't affect the test. Other defaults
	// (MaxDeliver / AckWait / ReadBatch) get applied at
	// NewRedisBus time; we inspect via the bus's stored
	// opts.
	bus, err := NewRedisBus(RedisBusOptions{
		DSN:          "redis://localhost:6379",
		GroupPrefix:  "users",
		ConsumerName: "test-replica",
		// MaxDeliver / AckWait / ReadBatch deliberately zero.
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v (DSN parse succeeded, defaults should still apply)", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	if bus.opts.DefaultMaxDeliver != 3 {
		t.Errorf("default MaxDeliver = %d, want 3", bus.opts.DefaultMaxDeliver)
	}
	if bus.opts.DefaultAckWait != 30*time.Second {
		t.Errorf("default AckWait = %v, want 30s", bus.opts.DefaultAckWait)
	}
	if bus.opts.ReadBatch != 10 {
		t.Errorf("default ReadBatch = %d, want 10", bus.opts.ReadBatch)
	}
}

func TestNewRedisBus_ConsumerNameDefaultsToHostname(t *testing.T) {
	// When ConsumerName is empty, NewRedisBus falls back to
	// os.Hostname() (or "wc-subscriber" on hostname error).
	bus, err := NewRedisBus(RedisBusOptions{
		DSN:         "redis://localhost:6379",
		GroupPrefix: "users",
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	if bus.opts.ConsumerName == "" {
		t.Error("ConsumerName should be auto-populated, got empty")
	}
}

func TestDLQStream(t *testing.T) {
	cases := []struct {
		channel string
		want    string
	}{
		{"default", "default.dlq"},
		{"audit", "audit.dlq"},
		{"telemetry", "telemetry.dlq"},
		// Edge case — empty channel still produces a
		// deterministic stream key. Not a meaningful runtime
		// case but the helper handles it.
		{"", ".dlq"},
	}
	for _, c := range cases {
		if got := dlqStream(c.channel); got != c.want {
			t.Errorf("dlqStream(%q) = %q, want %q", c.channel, got, c.want)
		}
	}
}

func TestRedisFieldNames(t *testing.T) {
	// Asserting the field constants are what XADD producers
	// emit + XREADGROUP consumers extract. Locks the wire
	// contract — if these change the cross-version
	// compatibility breaks, so the test exists to surface
	// any accidental rename.
	if redisFieldTopic != "topic" {
		t.Errorf("redisFieldTopic = %q, want %q", redisFieldTopic, "topic")
	}
	if redisFieldPayload != "payload" {
		t.Errorf("redisFieldPayload = %q, want %q", redisFieldPayload, "payload")
	}
}
