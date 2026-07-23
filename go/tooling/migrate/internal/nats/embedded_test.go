package nats_test

import (
	"context"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
)

// liveApplier boots an in-process NATS server WITH JetStream
// (random free port, throwaway store dir) and returns an Applier
// wired to it. The server URL is returned for tests that need a
// direct JetStream client to set up / assert state out-of-band.
func liveApplier(t *testing.T) (*nats.Applier, string) {
	t.Helper()
	opts := natsserver.DefaultTestOptions
	opts.Port = -1 // random free port
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	url := srv.ClientURL()
	a, err := nats.New(context.Background(), url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, url
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// directJS opens an independent JetStream context for test setup /
// assertions, so the Applier's own lazy connection isn't the only
// observer of server state.
func directJS(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("direct connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("direct jetstream: %v", err)
	}
	return js
}

// TestAppliedHead_Live — INVARIANT: AppliedHead returns "" when the
// tracker bucket is absent or empty, and the lexicographically-max
// key once it is populated (w17 ids are lex == chrono sortable).
func TestAppliedHead_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)

	// No bucket yet → "".
	head, err := a.AppliedHead(ctx)
	if err != nil {
		t.Fatalf("AppliedHead (no bucket): %v", err)
	}
	if head != "" {
		t.Errorf("absent-bucket head = %q, want \"\"", head)
	}

	// Create the bucket but leave it empty → ErrNoKeysFound → "".
	js := directJS(t, url)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "wc-migrations"})
	if err != nil {
		t.Fatalf("CreateKeyValue: %v", err)
	}
	head, err = a.AppliedHead(ctx)
	if err != nil {
		t.Fatalf("AppliedHead (empty bucket): %v", err)
	}
	if head != "" {
		t.Errorf("empty-bucket head = %q, want \"\"", head)
	}

	// Populate → newest wins.
	for _, k := range []string{"20260101T000000Z", "20260301T120000Z", "20260201T060000Z"} {
		if _, err := kv.Put(ctx, k, []byte("hash")); err != nil {
			t.Fatalf("kv put %s: %v", k, err)
		}
	}
	head, err = a.AppliedHead(ctx)
	if err != nil {
		t.Fatalf("AppliedHead (populated): %v", err)
	}
	if head != "20260301T120000Z" {
		t.Errorf("head = %q, want newest", head)
	}
}

// TestApply_KvAddPutAndAppliedHead_Live — INVARIANT: an Apply body
// that creates the tracker bucket and puts a key drives connect →
// runKv(add) → runKv(put); the value is observable and AppliedHead
// reflects it.
func TestApply_KvAddPutAndAppliedHead_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)

	err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "20260401T000000Z",
		UpSql: "# wc: marker\n" +
			"nats kv add wc-migrations\n" +
			"nats kv put wc-migrations 20260401T000000Z deadbeef",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	js := directJS(t, url)
	kv, err := js.KeyValue(ctx, "wc-migrations")
	if err != nil {
		t.Fatalf("KeyValue: %v", err)
	}
	entry, err := kv.Get(ctx, "20260401T000000Z")
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	if string(entry.Value()) != "deadbeef" {
		t.Errorf("put value = %q, want deadbeef", string(entry.Value()))
	}
	head, err := a.AppliedHead(ctx)
	if err != nil {
		t.Fatalf("AppliedHead: %v", err)
	}
	if head != "20260401T000000Z" {
		t.Errorf("head = %q", head)
	}
}

// TestApply_KvAddIdempotent_Live — INVARIANT: re-creating an
// existing bucket is non-fatal (ErrBucketExists soft-success), so a
// re-applied initial migration succeeds.
func TestApply_KvAddIdempotent_Live(t *testing.T) {
	a, _ := liveApplier(t)
	ctx := liveCtx(t)
	body := "nats kv add wc-migrations"
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "a", UpSql: body}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "a", UpSql: body}); err != nil {
		t.Errorf("second add should be a no-op, got %v", err)
	}
}

// TestApply_KvPutReusesCachedBucket_Live — INVARIANT: two puts to
// the same bucket reuse the cached KV handle (second put goes
// through the cache-hit path in bucket()).
func TestApply_KvPutReusesCachedBucket_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)
	err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "b",
		UpSql: "nats kv add data\n" +
			"nats kv put data k1 v1\n" +
			"nats kv put data k2 v2",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	js := directJS(t, url)
	kv, _ := js.KeyValue(ctx, "data")
	for k, want := range map[string]string{"k1": "v1", "k2": "v2"} {
		e, err := kv.Get(ctx, k)
		if err != nil {
			t.Fatalf("get %s: %v", k, err)
		}
		if string(e.Value()) != want {
			t.Errorf("%s = %q, want %q", k, string(e.Value()), want)
		}
	}
}

// TestRollback_KvDel_Live — INVARIANT: the down path deletes a
// tracker key; deleting an already-missing key (and a missing
// bucket) is tolerated as soft-success.
func TestRollback_KvDel_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)
	js := directJS(t, url)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "wc-migrations"})
	if err != nil {
		t.Fatalf("CreateKeyValue: %v", err)
	}
	if _, err := kv.Put(ctx, "20260401T000000Z", []byte("x")); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	err = a.Rollback(ctx, &applyfetchpb.Migration{
		Id:      "20260401T000000Z",
		DownSql: "nats kv del wc-migrations 20260401T000000Z",
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := kv.Get(ctx, "20260401T000000Z"); err == nil {
		t.Error("key should have been deleted")
	}

	// Idempotent re-delete (key now missing) + delete against a
	// bucket that doesn't exist — both tolerated.
	err = a.Rollback(ctx, &applyfetchpb.Migration{
		Id: "x",
		DownSql: "nats kv del wc-migrations 20260401T000000Z\n" +
			"nats kv del nonexistent-bucket somekey",
	})
	if err != nil {
		t.Errorf("idempotent/ missing-bucket del should be a no-op, got %v", err)
	}
}

// TestApply_KvPurge_Live — INVARIANT: purge drops every key in a
// bucket without removing the bucket itself; purge on a missing
// bucket is tolerated.
func TestApply_KvPurge_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)
	js := directJS(t, url)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "junk"})
	if err != nil {
		t.Fatalf("CreateKeyValue: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, err := kv.Put(ctx, k, []byte("1")); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	err = a.Apply(ctx, &applyfetchpb.Migration{
		Id: "p",
		UpSql: "nats kv purge junk\n" +
			"nats kv purge never-existed",
	})
	if err != nil {
		t.Fatalf("Apply purge: %v", err)
	}
	if _, err := kv.Keys(ctx); err == nil {
		t.Error("bucket should have no keys after purge")
	}
}

// TestApply_KvRm_Live — INVARIANT: kv rm deletes the bucket and
// evicts the cached handle; rm against a missing bucket is
// tolerated.
func TestApply_KvRm_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)
	js := directJS(t, url)
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "temp"}); err != nil {
		t.Fatalf("CreateKeyValue: %v", err)
	}
	err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "r",
		UpSql: "nats kv rm temp\n" +
			"nats kv rm already-gone",
	})
	if err != nil {
		t.Fatalf("Apply rm: %v", err)
	}
	if _, err := js.KeyValue(ctx, "temp"); err == nil {
		t.Error("bucket temp should have been removed")
	}
}

// TestStreamEdit_Live — INVARIANT (guard): a legitimate stream edit
// against an EXISTING stream still reads the config, sets the
// description, and writes it back via UpdateStream. (The
// missing-stream `ghost` line was removed from this body: a
// `stream edit` against a nonexistent stream is now a real failure,
// not a tolerated no-op — see TestStreamEditMissingStreamErrors_Live.)
func TestStreamEdit_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)
	js := directJS(t, url)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "users",
		Subjects: []string{"users.>"},
	}); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}

	err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "e",
		UpSql: `nats stream edit users --description "renamed to people"`,
	})
	if err != nil {
		t.Fatalf("Apply stream edit: %v", err)
	}
	s, err := js.Stream(ctx, "users")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := s.CachedInfo().Config.Description; got != "renamed to people" {
		t.Errorf("description = %q, want \"renamed to people\"", got)
	}
}

// TestStreamEditMissingStreamErrors_Live — INVARIANT: a `stream edit`
// targeting a stream that does not exist is a REAL failure, not a
// swallowed no-op. The old handler returned nil on ErrStreamNotFound,
// which meant a migration editing a nonexistent stream reported
// success. Assert it now errors and names the missing stream.
func TestStreamEditMissingStreamErrors_Live(t *testing.T) {
	a, _ := liveApplier(t)
	ctx := liveCtx(t)
	err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "ghost",
		UpSql: `nats stream edit ghost --description "no such stream"`,
	})
	if err == nil {
		t.Fatal("editing a missing stream should error, got nil (swallowed as success)")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the missing stream, got: %v", err)
	}
}

// TestStreamRm_Live — INVARIANT: stream rm deletes the stream
// (--force stripped); rm against a missing stream is tolerated.
func TestStreamRm_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)
	js := directJS(t, url)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "orders",
		Subjects: []string{"orders.>"},
	}); err != nil {
		t.Fatalf("CreateStream: %v", err)
	}
	err := a.Rollback(ctx, &applyfetchpb.Migration{
		Id: "rm",
		DownSql: "nats stream rm --force orders\n" +
			"nats stream rm --force never-existed\n" +
			"nats stream ls",
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := js.Stream(ctx, "orders"); err == nil {
		t.Error("stream orders should have been removed")
	}
}

// TestRunOne_DispatchErrors_Live — INVARIANT: malformed / unsupported
// command lines surface a descriptive error once the connection is
// live (these branches sit after jsContext()).
func TestRunOne_DispatchErrors_Live(t *testing.T) {
	a, _ := liveApplier(t)
	ctx := liveCtx(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unsupported top", "nats frobnicate x", "unsupported subcommand"},
		{"kv missing sub", "nats kv", "missing subcommand"},
		{"kv unsupported sub", "nats kv frob bucket", "unsupported subcommand"},
		{"kv add missing bucket", "nats kv add", "missing bucket name"},
		{"kv rm missing bucket", "nats kv rm", "missing bucket name"},
		{"kv put too few args", "nats kv put bucket key", "expected"},
		{"kv del too few args", "nats kv del bucket", "expected"},
		{"kv purge missing bucket name", "nats kv purge", "missing bucket name"},
		{"stream missing sub", "nats stream", "missing subcommand"},
		{"stream unsupported sub", "nats stream frob x", "unsupported subcommand"},
		{"stream rm missing name", "nats stream rm --force", "missing stream name"},
		{"stream edit missing name", "nats stream edit", "missing stream name"},
		{"stream edit no description", "nats stream edit users --color blue", "only --description supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.Apply(ctx, &applyfetchpb.Migration{Id: tc.name, UpSql: tc.body})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("body %q: got %v, want error containing %q", tc.body, err, tc.want)
			}
		})
	}
}
