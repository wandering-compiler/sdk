package nats_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
)

// bootNATS starts an in-process NATS+JetStream server and returns its URL.
func bootNATS(t *testing.T) string {
	t.Helper()
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// jsFor opens a direct JetStream client against url for out-of-band setup
// and assertions.
func jsFor(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

// Dump mirrors every KV bucket (incl. empty ones) + latest values; Restore
// recreates them. Round-trip across two servers proves both directions.
func TestSnapshotter_DumpRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcURL := bootNATS(t)
	srcJS := jsFor(t, srcURL)

	cfg, err := srcJS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Put(ctx, "alpha", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Put(ctx, "beta", []byte("2")); err != nil {
		t.Fatal(err)
	}
	// An empty bucket — exercises the bucket-marker + ErrNoKeysFound arm.
	if _, err := srcJS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "blank"}); err != nil {
		t.Fatal(err)
	}

	src, err := nats.NewSnapshotter(srcURL)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.Dump(ctx, &buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("Dump produced no bytes")
	}

	dstURL := bootNATS(t)
	dst, err := nats.NewSnapshotter(dstURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	dstJS := jsFor(t, dstURL)
	rcfg, err := dstJS.KeyValue(ctx, "cfg")
	if err != nil {
		t.Fatalf("restored cfg bucket: %v", err)
	}
	e, err := rcfg.Get(ctx, "alpha")
	if err != nil {
		t.Fatalf("restored alpha: %v", err)
	}
	if string(e.Value()) != "1" {
		t.Errorf("alpha = %q, want 1", e.Value())
	}
	if _, err := dstJS.KeyValue(ctx, "blank"); err != nil {
		t.Errorf("empty bucket not restored: %v", err)
	}

	// Restoring the same dump again hits the ErrBucketExists rebind arm.
	if err := dst.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Restore (idempotent re-run): %v", err)
	}
}

// Restore surfaces a malformed gob stream; an empty stream is a clean EOF.
func TestSnapshotter_RestoreDecodeError(t *testing.T) {
	s, err := nats.NewSnapshotter(bootNATS(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(context.Background(), strings.NewReader("garbage")); err == nil {
		t.Fatal("want decode error on a malformed stream")
	}
	if err := s.Restore(context.Background(), bytes.NewReader(nil)); err != nil {
		t.Fatalf("empty stream should be a no-op, got %v", err)
	}
}

// Dump on a server with no KV buckets is a clean no-op.
func TestSnapshotter_DumpEmpty(t *testing.T) {
	s, err := nats.NewSnapshotter(bootNATS(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dump(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatalf("Dump empty: %v", err)
	}
}
