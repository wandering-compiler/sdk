package redis_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
)

func snapFor(t *testing.T, mr *miniredis.Miniredis) *redis.Snapshotter {
	t.Helper()
	s, err := redis.NewSnapshotter("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	return s
}

// Dump SCANs the whole keyspace + per-key TTL; Restore RESTOREs each back.
// The round-trip across two miniredis servers proves both directions and
// the TTL-preserving branch.
func TestSnapshotter_DumpRestoreRoundTrip(t *testing.T) {
	src := miniredis.RunT(t)
	src.Set("plain", "v1")
	src.Set("withttl", "v2")
	src.SetTTL("withttl", 5*time.Minute)

	var buf bytes.Buffer
	if err := snapFor(t, src).Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("Dump produced no bytes")
	}

	dst := miniredis.RunT(t)
	if err := snapFor(t, dst).Restore(context.Background(), &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if v, _ := dst.Get("plain"); v != "v1" {
		t.Errorf("plain = %q, want v1", v)
	}
	if v, _ := dst.Get("withttl"); v != "v2" {
		t.Errorf("withttl = %q, want v2", v)
	}
	// The TTL survived the round-trip (some expiry was set).
	if ttl := dst.TTL("withttl"); ttl <= 0 {
		t.Errorf("withttl TTL not preserved: %v", ttl)
	}
}

// Dump on an empty keyspace is a clean no-op producing an empty stream.
func TestSnapshotter_DumpEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := snapFor(t, miniredis.RunT(t)).Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump empty: %v", err)
	}
}

// Restore surfaces a malformed gob stream; an empty stream is a clean EOF.
func TestSnapshotter_RestoreDecodeError(t *testing.T) {
	s := snapFor(t, miniredis.RunT(t))
	if err := s.Restore(context.Background(), strings.NewReader("garbage")); err == nil {
		t.Fatal("want decode error on a malformed stream")
	}
	if err := s.Restore(context.Background(), bytes.NewReader(nil)); err != nil {
		t.Fatalf("empty stream should be a no-op, got %v", err)
	}
}

// Wipe FLUSHDBs the keyspace, leaving it empty.
func TestWipe_FlushesKeyspace(t *testing.T) {
	a, mr := liveApplier(t)
	mr.Set("a", "1")
	mr.Set("b", "2")
	if err := a.Wipe(context.Background()); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Errorf("keyspace not empty after Wipe: %v", keys)
	}
}
