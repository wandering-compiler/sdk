package eventbus

// White-box unit tests for the Redis Streams adapter's
// validation + pure-function helpers. Live-broker integration
// deferred to a follow-up slice via the existing
// the container-based integration pattern.

import (
	"context"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
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
		DSN:          "redis://" + miniredis.RunT(t).Addr(),
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
	// os.Hostname() (or "w17-subscriber" on hostname error).
	bus, err := NewRedisBus(RedisBusOptions{
		DSN:         "redis://" + miniredis.RunT(t).Addr(),
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

// NewRedisBus is documented to connect up front. Against a broker that is
// not there it must fail at construction, not hand back a bus whose first
// emit discovers the outage.
func TestNewRedisBus_UnreachableBrokerFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens on addr now

	bus, err := NewRedisBus(RedisBusOptions{
		DSN:         "redis://:hunter2@" + addr + "?dial_timeout=500ms",
		GroupPrefix: "users",
	})
	if err == nil {
		_ = bus.Close(context.Background())
		t.Fatal("NewRedisBus against an unreachable broker returned a bus and no error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("connection error quotes the DSN password: %v", err)
	}
}

// The consume and pending-scan loops must report a broker failure (once per
// outage, not once per 200ms retry) and log when it clears.
func TestRedisSubscriber_LoopFailuresAreReported(t *testing.T) {
	var buf syncBuffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	mr := miniredis.RunT(t)
	bus, err := NewRedisBus(RedisBusOptions{
		DSN:            "redis://" + mr.Addr() + "?max_retries=-1&dial_timeout=200ms",
		GroupPrefix:    "loopfail",
		ConsumerName:   "r1",
		DefaultAckWait: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	sub, err := bus.Subscriber(context.Background(), "ch")
	if err != nil {
		t.Fatalf("Subscriber: %v", err)
	}
	if err := sub.Subscribe(context.Background(), "**", func(context.Context, string, []byte) error { return nil }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	mr.SetError("ERR simulated outage")
	time.Sleep(1200 * time.Millisecond) // several 200ms consume retries, many 50ms scans
	mr.SetError("")
	time.Sleep(1500 * time.Millisecond) // XREADGROUP blocks up to 1s

	out := buf.String()
	for _, stage := range []string{"redis consume", "redis pending scan"} {
		// reportBusFailure writes one log line (plus the error reporter's
		// own, prefixed "error: "); count the log line.
		n := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "eventbus "+stage+" failed") && !strings.Contains(line, "error: eventbus") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q failure reported %d times, want once per outage; log:\n%s", stage, n, out)
		}
		if !strings.Contains(out, "eventbus "+stage+" recovered") {
			t.Errorf("%q recovery not logged; log:\n%s", stage, out)
		}
	}
	if !strings.Contains(out, "simulated outage") {
		t.Errorf("reported failure does not carry the broker error; log:\n%s", out)
	}
}

// syncBuffer is a goroutine-safe log sink: the loops under test write from
// their own goroutines while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
