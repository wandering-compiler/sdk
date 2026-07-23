package nats_test

import (
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// runKv argument-validation + unsupported-subcommand errors. Each runs
// through a live Apply (connect → run → do → runKv) so the per-subcommand
// guard arms are exercised.
func TestApply_KvArgErrors_Live(t *testing.T) {
	a, _ := liveApplier(t)
	ctx := liveCtx(t)
	cases := map[string]string{
		"nats kv add":            "missing bucket name",
		"nats kv rm":             "missing bucket name",
		"nats kv put onlybucket": "expected",
		"nats kv del onlybucket": "expected",
		"nats kv purge":          "missing bucket name",
		"nats kv frobnicate x":   "unsupported subcommand",
	}
	for body, want := range cases {
		t.Run(body, func(t *testing.T) {
			err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: body})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Apply(%q) = %v, want error containing %q", body, err, want)
			}
		})
	}
}

// runStream: edit a stream's description (read-modify-write), rm it, the
// not-found tolerances, and the argument/subcommand guards.
func TestApply_StreamScenarios_Live(t *testing.T) {
	a, url := liveApplier(t)
	ctx := liveCtx(t)
	js := directJS(t, url)

	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "users", Subjects: []string{"users.>"},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	// edit --description (read-modify-write happy path).
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		UpSql: `nats stream edit users --description "renamed"`,
	}); err != nil {
		t.Fatalf("stream edit: %v", err)
	}
	s, err := js.Stream(ctx, "users")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CachedInfo().Config.Description; got != "renamed" {
		t.Errorf("description = %q, want renamed", got)
	}

	// ls is a no-op.
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		UpSql: "nats stream ls",
	}); err != nil {
		t.Fatalf("ls: %v", err)
	}

	// edit on a missing stream is now a REAL failure (was tolerated
	// as a not-found no-op) — a stream edit that mutates nothing must
	// not report success.
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		UpSql: `nats stream edit ghost --description "x"`,
	}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("edit-missing = %v, want error containing \"does not exist\"", err)
	}

	// rm --force the stream, then rm a non-existent one (tolerated).
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		UpSql: "nats stream rm --force users\nnats stream rm never-existed",
	}); err != nil {
		t.Fatalf("stream rm: %v", err)
	}
	if _, err := js.Stream(ctx, "users"); err == nil {
		t.Error("stream users should be gone after rm")
	}
}

// runStream guard arms: missing subcommand args + unsupported flags.
func TestApply_StreamArgErrors_Live(t *testing.T) {
	a, _ := liveApplier(t)
	ctx := liveCtx(t)
	cases := map[string]string{
		"nats stream rm":               "missing stream name",
		"nats stream edit":             "missing stream name",
		"nats stream edit users":       "only --description supported",
		"nats stream frobnicate users": "unsupported subcommand",
	}
	for body, want := range cases {
		t.Run(body, func(t *testing.T) {
			err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: body})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Apply(%q) = %v, want error containing %q", body, err, want)
			}
		})
	}
}
