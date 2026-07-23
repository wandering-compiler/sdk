//go:build dockertest

package nats_test

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
)

// TestSnapshotter_RoundTrip is the S2 nats verify: seed KV buckets →
// dump → delete buckets → restore → keys + values (+ empty bucket)
// preserved. Documents the lossy scope (latest revision only; streams
// not captured). Gated by `//go:build dockertest`.
func TestSnapshotter_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := startThrowawayNATS(ctx, t)
	js := connectJS(ctx, t, dsn)

	// Seed: one populated bucket + one empty bucket.
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "settings"})
	if err != nil {
		t.Fatalf("create settings: %v", err)
	}
	if _, err := kv.PutString(ctx, "theme", "dark"); err != nil {
		t.Fatalf("put theme: %v", err)
	}
	if _, err := kv.PutString(ctx, "lang", "en"); err != nil {
		t.Fatalf("put lang: %v", err)
	}
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "empty"}); err != nil {
		t.Fatalf("create empty: %v", err)
	}

	snap, err := nats.NewSnapshotter(dsn)
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

	// Wipe: delete both buckets.
	if err := js.DeleteKeyValue(ctx, "settings"); err != nil {
		t.Fatalf("delete settings: %v", err)
	}
	if err := js.DeleteKeyValue(ctx, "empty"); err != nil {
		t.Fatalf("delete empty: %v", err)
	}

	if err := snap.Restore(ctx, bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// settings restored with its keys.
	rkv, err := js.KeyValue(ctx, "settings")
	if err != nil {
		t.Fatalf("reopen settings: %v", err)
	}
	if e, err := rkv.Get(ctx, "theme"); err != nil || string(e.Value()) != "dark" {
		t.Errorf("theme = %q (err %v), want dark", valueOf(e), err)
	}
	if e, err := rkv.Get(ctx, "lang"); err != nil || string(e.Value()) != "en" {
		t.Errorf("lang = %q (err %v), want en", valueOf(e), err)
	}
	// empty bucket recreated.
	if _, err := js.KeyValue(ctx, "empty"); err != nil {
		t.Errorf("empty bucket not restored: %v", err)
	}
}

func valueOf(e jetstream.KeyValueEntry) string {
	if e == nil {
		return ""
	}
	return string(e.Value())
}

func connectJS(ctx context.Context, t *testing.T, dsn string) jetstream.JetStream {
	t.Helper()
	nc, err := natsgo.Connect(dsn)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

func startThrowawayNATS(ctx context.Context, t *testing.T) string {
	t.Helper()
	// -js enables JetStream (required for KV buckets).
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "-P",
		"nats:2-alpine", "-js").Output()
	if err != nil {
		t.Skipf("docker run failed (no docker?): %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "stop", id).Run() })

	portOut, err := exec.CommandContext(ctx, "docker", "port", id, "4222/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	first := strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0]
	port := first[strings.LastIndex(first, ":")+1:]
	dsn := fmt.Sprintf("nats://127.0.0.1:%s", port)

	deadline := time.Now().Add(60 * time.Second)
	for {
		nc, err := natsgo.Connect(dsn)
		if err == nil {
			nc.Close()
			return dsn
		}
		if time.Now().After(deadline) {
			t.Fatalf("nats not ready: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
