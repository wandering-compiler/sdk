package nats_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
)

// The operation-fails-mid-call error arms (a JetStream call returning a
// non-sentinel error) are normally only reachable when the server misbehaves.
// A pre-cancelled context drives them deterministically and fast: each JS
// call returns context.Canceled — which is NOT ErrBucketNotFound /
// ErrNoKeysFound / ErrBucketExists — so the generic error wraps fire.
func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestNats_OperationErrorArms(t *testing.T) {
	a, _ := liveApplier(t)

	// Connect for real + cache the "data" bucket so a later put hits the
	// cached-handle path (kv.Put error) rather than the bucket-open error.
	if err := a.Apply(liveCtx(t), &applyfetchpb.Migration{
		UpSql: "nats kv add data\nnats kv put data k v",
	}); err != nil {
		t.Fatalf("setup apply: %v", err)
	}

	cx := cancelled()
	cases := []struct {
		name, body, want string
	}{
		{"kv add", "nats kv add freshbucket", "kv add"},
		{"kv put cached", "nats kv put data k2 v2", "kv put"},
		{"kv put open", "nats kv put otherbucket k v", "KeyValue"},
		{"kv rm", "nats kv rm somebucket", "kv rm"},
		{"kv del open", "nats kv del db3 key", "KeyValue"},
		{"kv del cached", "nats kv del data k", "kv del"}, // cached bucket → Delete error arm
		{"kv purge open", "nats kv purge db4", "KeyValue"},
		{"kv purge cached", "nats kv purge data", "kv keys"}, // cached bucket → Keys error arm
		{"stream rm", "nats stream rm astream", "stream rm"},
		{"stream edit", `nats stream edit astream --description "x"`, "stream lookup"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := a.Apply(cx, &applyfetchpb.Migration{UpSql: c.body})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Apply(%q) = %v, want error containing %q", c.body, err, c.want)
			}
		})
	}
}

// Restore's create-bucket error arm: feed a real dump stream but with a
// cancelled context, so CreateKeyValue fails on the bucket marker.
func TestNats_RestoreErrorArm(t *testing.T) {
	ctx := liveCtx(t)
	// Seed + dump a bucket from a live server with a good context.
	_, srcURL := liveApplier(t)
	srcJS := jsFor(t, srcURL)
	if _, err := srcJS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "cfg"}); err != nil {
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

	_, dstURL := liveApplier(t)
	dst, err := nats.NewSnapshotter(dstURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Restore(cancelled(), bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("want Restore error under a cancelled context")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, context.Canceled }

// Dump's encode-error arm: a failing writer makes the per-bucket gob encode
// fail once a KV bucket is listed.
func TestNats_DumpEncodeError(t *testing.T) {
	ctx := liveCtx(t)
	_, url := liveApplier(t)
	js := jsFor(t, url)
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "cfg"}); err != nil {
		t.Fatal(err)
	}
	s, err := nats.NewSnapshotter(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dump(context.Background(), errWriter{}); err == nil {
		t.Fatal("want encode error from a failing writer")
	}
}

// AppliedHead's KeyValue-error arm (a non-NotFound failure).
func TestNats_AppliedHeadErrorArm(t *testing.T) {
	a, _ := liveApplier(t)
	if _, err := a.AppliedHead(liveCtx(t)); err != nil { // prime the connection
		t.Fatalf("prime AppliedHead: %v", err)
	}
	if _, err := a.AppliedHead(cancelled()); err == nil {
		t.Fatal("want AppliedHead error under a cancelled context")
	}
}

// Dump's bucket-listing error arm: a cancelled context makes the KV-store
// listing surface an error rather than completing. (connect() takes no ctx,
// so the Snapshotter still dials the live server, then the listing fails.)
func TestNats_DumpErrorArm(t *testing.T) {
	_, url := liveApplier(t)
	s, err := nats.NewSnapshotter(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dump(cancelled(), &bytes.Buffer{}); err == nil {
		t.Fatal("want Dump error under a cancelled context")
	}
}
