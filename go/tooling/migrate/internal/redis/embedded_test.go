package redis_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
)

// liveApplier spins up an in-process miniredis (a real RESP
// server) and returns an Applier wired to it plus the server
// handle for white-box state assertions. Both are torn down via
// t.Cleanup.
func liveApplier(t *testing.T) (*redis.Applier, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	a, err := redis.New(context.Background(), "redis://"+mr.Addr())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, mr
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestAppliedHead_EmptyAndPopulated_Live — INVARIANT: AppliedHead
// returns "" against a virgin store (no bookkeeping hash yet) and
// the lexicographically-max member once the hash is populated
// (w17's id format is lex == chrono sortable, so max-lex == newest).
func TestAppliedHead_EmptyAndPopulated_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)

	head, err := a.AppliedHead(ctx)
	if err != nil {
		t.Fatalf("AppliedHead (empty): %v", err)
	}
	if head != "" {
		t.Errorf("empty store head = %q, want \"\"", head)
	}

	mr.HSet("wc:migrations", "20260101T000000Z", "aa")
	mr.HSet("wc:migrations", "20260301T120000Z", "bb")
	mr.HSet("wc:migrations", "20260201T060000Z", "cc")

	head, err = a.AppliedHead(ctx)
	if err != nil {
		t.Fatalf("AppliedHead (populated): %v", err)
	}
	if head != "20260301T120000Z" {
		t.Errorf("head = %q, want newest", head)
	}
}

// TestApply_CommandBody_Live — INVARIANT: a command-style body
// dispatches each line through run→do against the live server;
// up_sql and up_post_tx both run, comment + blank lines are
// no-ops, and the resulting hash entries are observable.
func TestApply_CommandBody_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)

	err := a.Apply(ctx, &applyfetchpb.Migration{
		Id:       "20260401T000000Z",
		UpSql:    "# wc: bookkeeping\nHSET wc:migrations 20260401T000000Z deadbeef\n\nSET marker one",
		UpPostTx: "HSET wc:migrations 20260401T000001Z cafef00d",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := mr.HGet("wc:migrations", "20260401T000000Z"); got != "deadbeef" {
		t.Errorf("up_sql HSET = %q, want deadbeef", got)
	}
	if got := mr.HGet("wc:migrations", "20260401T000001Z"); got != "cafef00d" {
		t.Errorf("up_post_tx HSET = %q, want cafef00d", got)
	}
	if got, _ := mr.Get("marker"); got != "one" {
		t.Errorf("SET marker = %q, want one", got)
	}
}

// TestApply_DoNilResultIsSuccess_Live — INVARIANT: a command that
// yields redis.Nil (here GET on a missing key) is treated as
// success by do(), so the migration as a whole succeeds.
func TestApply_DoNilResultIsSuccess_Live(t *testing.T) {
	a, _ := liveApplier(t)
	if err := a.Apply(liveCtx(t), &applyfetchpb.Migration{
		Id:    "20260401T000002Z",
		UpSql: "GET definitely-missing-key",
	}); err != nil {
		t.Errorf("GET-missing body should succeed (Nil==success), got %v", err)
	}
}

// TestRollback_CommandBody_Live — INVARIANT: the down path runs
// down_pre_tx then down_sql; an HDEL erases the bookkeeping row.
func TestRollback_CommandBody_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	mr.HSet("wc:migrations", "20260401T000003Z", "feed")
	_ = mr.Set("leftover", "x")

	err := a.Rollback(ctx, &applyfetchpb.Migration{
		Id:        "20260401T000003Z",
		DownPreTx: "DEL leftover",
		DownSql:   "HDEL wc:migrations 20260401T000003Z",
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if mr.Exists("leftover") {
		t.Error("down_pre_tx DEL did not run")
	}
	if mr.HGet("wc:migrations", "20260401T000003Z") != "" {
		t.Error("down_sql HDEL did not erase bookkeeping row")
	}
}

// TestApply_CommandBody_ExecErrorSurfaces_Live — INVARIANT: a
// server-rejected command (wrong arity for a real command) surfaces
// as an exec error naming the offending line — not a silent skip.
func TestApply_CommandBody_ExecErrorSurfaces_Live(t *testing.T) {
	a, _ := liveApplier(t)
	err := a.Apply(liveCtx(t), &applyfetchpb.Migration{
		Id:    "20260401T000004Z",
		UpSql: "HSET wc:migrations", // missing field+value → server error
	})
	if err == nil || !strings.Contains(err.Error(), "up_sql") {
		t.Errorf("expected up_sql exec error, got %v", err)
	}
}

const yamlAddActive = `version: 1
encoding: json
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: users:*
    field: active
    value: true
`

// TestApply_YAMLDataMigration_Live — INVARIANT: a YAML
// ADD_FIELD_DEFAULT body SCANs the keyspace, rewrites each JSON
// value, records bookkeeping in wc:migrations, and leaves NO
// cursor object behind on a clean end-to-end run.
func TestApply_YAMLDataMigration_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("users:1", `{"name":"a"}`)
	_ = mr.Set("users:2", `{"name":"b","active":false}`) // already has field
	_ = mr.Set("other:1", `{"name":"c"}`)                // outside keyspace

	m := &applyfetchpb.Migration{Id: "20260501T000000Z", UpSql: yamlAddActive}
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("Apply YAML: %v", err)
	}

	if got, _ := mr.Get("users:1"); !strings.Contains(got, `"active":true`) {
		t.Errorf("users:1 not migrated: %q", got)
	}
	// users:2 already had active:false → ADD_FIELD_DEFAULT is a
	// no-op (changed=false), value preserved.
	if got, _ := mr.Get("users:2"); !strings.Contains(got, `"active":false`) {
		t.Errorf("users:2 should be untouched: %q", got)
	}
	if got, _ := mr.Get("other:1"); strings.Contains(got, "active") {
		t.Errorf("other:1 outside keyspace should be untouched: %q", got)
	}
	if mr.HGet("wc:migrations", "20260501T000000Z") == "" {
		t.Error("bookkeeping row not written")
	}
	if mr.Exists("wc:data-migrations:20260501T000000Z") {
		t.Error("cursor object should be cleared on clean run")
	}
}

// TestApply_YAMLPreservesTTL_Live — INVARIANT: a data op's per-key
// rewrite preserves an existing expiry. applyOpToKey re-SETs the
// mutated value with KEEPTTL, so a key that carried a TTL stays
// volatile instead of being silently made immortal by a plain SET.
func TestApply_YAMLPreservesTTL_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("users:1", `{"name":"a"}`)
	mr.SetTTL("users:1", time.Hour)

	m := &applyfetchpb.Migration{Id: "20260501T000010Z", UpSql: yamlAddActive}
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, _ := mr.Get("users:1"); !strings.Contains(got, `"active":true`) {
		t.Fatalf("users:1 not migrated: %q", got)
	}
	if ttl := mr.TTL("users:1"); ttl <= 0 {
		t.Errorf("rewrite stripped the TTL: got %v, want > 0", ttl)
	}
}

// TestApply_YAMLIdempotent_Live — INVARIANT: re-applying a
// completed YAML migration is a safe no-op — per-key idempotency
// means no value changes, and the call still succeeds.
func TestApply_YAMLIdempotent_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("users:1", `{"name":"a"}`)
	m := &applyfetchpb.Migration{Id: "20260501T000001Z", UpSql: yamlAddActive}

	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first, _ := mr.Get("users:1")
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	second, _ := mr.Get("users:1")
	if first != second {
		t.Errorf("re-apply changed value: %q -> %q", first, second)
	}
}

// TestApply_YAMLResumability_Live — INVARIANT: a pre-existing
// cursor entry skips the already-completed op at op-granularity;
// only the not-yet-recorded op runs on resume.
func TestApply_YAMLResumability_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("users:1", `{"name":"a"}`)

	body := `version: 1
encoding: json
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: users:*
    field: op0
    value: true
  - op: ADD_FIELD_DEFAULT
    keyspace: users:*
    field: op1
    value: true
`
	// Mark op 0 as already complete on the cursor side-channel.
	mr.HSet("wc:data-migrations:20260501T000002Z", "0", "complete")

	m := &applyfetchpb.Migration{Id: "20260501T000002Z", UpSql: body}
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := mr.Get("users:1")
	if strings.Contains(got, "op0") {
		t.Errorf("op0 should have been skipped via cursor, value=%q", got)
	}
	if !strings.Contains(got, "op1") {
		t.Errorf("op1 should have run, value=%q", got)
	}
}

// TestApply_YAMLTransformResumeRefused_Live — INVARIANT (V-11): a
// TRANSFORM_FIELD op whose cursor shows it started but never
// completed (a mid-op crash) is REFUSED on resume rather than
// silently re-run. The user's Starlark script may be non-idempotent
// (here `value + b"!"` appends every run), so the key the op already
// touched must stay byte-identical on the refused resume — proof no
// double-apply happens.
func TestApply_YAMLTransformResumeRefused_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("doc:1", "v")

	body := `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: doc:*
    script_lang: starlark
    script: |
      def transform(value):
          return value + b"!"
`
	// Simulate a crash mid-op: op 0's start marker is on the cursor
	// but it never recorded complete.
	mr.HSet("wc:data-migrations:20260501T000009Z", "started:0", "started")

	m := &applyfetchpb.Migration{Id: "20260501T000009Z", UpSql: body}
	err := a.Apply(ctx, m)
	if err == nil || !strings.Contains(err.Error(), "TRANSFORM_FIELD") {
		t.Fatalf("expected refusal naming TRANSFORM_FIELD, got %v", err)
	}
	if got, _ := mr.Get("doc:1"); got != "v" {
		t.Errorf("refused resume must not re-transform; doc:1 = %q, want v", got)
	}
}

// TestApply_YAMLTransformField_Live — INVARIANT: a TRANSFORM_FIELD
// op runs the compiled Starlark VM per key; a bytes return rewrites
// the value, a None return is an explicit no-op.
func TestApply_YAMLTransformField_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("doc:1", "hello")
	_ = mr.Set("doc:2", "skip")

	body := `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: doc:*
    script_lang: starlark
    script: |
      def transform(value):
          if value == b"skip":
              return None
          return b"migrated"
`
	m := &applyfetchpb.Migration{Id: "20260501T000003Z", UpSql: body}
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("Apply transform: %v", err)
	}
	if got, _ := mr.Get("doc:1"); got != "migrated" {
		t.Errorf("doc:1 transform = %q, want migrated", got)
	}
	if got, _ := mr.Get("doc:2"); got != "skip" {
		t.Errorf("doc:2 None-return should be untouched, got %q", got)
	}
}

// TestApply_YAMLParallelOverride_Live — INVARIANT: the operator
// --parallel override drives the worker fan-out; every key in a
// large keyspace is migrated regardless of worker count.
func TestApply_YAMLParallelOverride_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	a.SetParallelOverride(8)
	const n = 50
	for i := 0; i < n; i++ {
		_ = mr.Set("big:"+strconv.Itoa(i), `{"k":1}`)
	}
	m := &applyfetchpb.Migration{Id: "20260501T000004Z", UpSql: `version: 1
encoding: json
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: big:*
    field: active
    value: true
`}
	if err := a.Apply(ctx, m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for i := 0; i < n; i++ {
		got, _ := mr.Get("big:" + strconv.Itoa(i))
		if !strings.Contains(got, `"active":true`) {
			t.Fatalf("big:%d not migrated: %q", i, got)
			return
		}
	}
}

// TestApply_YAMLPerKeyDecodeError_Live — INVARIANT: a value that
// isn't valid JSON aborts the migration with the offending key in
// the error chain (the worker error propagates through runDataOp).
func TestApply_YAMLPerKeyDecodeError_Live(t *testing.T) {
	a, mr := liveApplier(t)
	_ = mr.Set("users:1", "this is not json")
	err := a.Apply(liveCtx(t), &applyfetchpb.Migration{
		Id:    "20260501T000005Z",
		UpSql: yamlAddActive,
	})
	if err == nil || !strings.Contains(err.Error(), "users:1") {
		t.Errorf("expected per-key decode error naming users:1, got %v", err)
	}
}

// TestRollback_YAMLInverse_Live — INVARIANT: a YAML down body runs
// the same op machinery in reverse, erases the bookkeeping row via
// HDEL, and clears its own (rollback-prefixed) cursor.
func TestRollback_YAMLInverse_Live(t *testing.T) {
	a, mr := liveApplier(t)
	ctx := liveCtx(t)
	_ = mr.Set("users:1", `{"name":"a","active":true}`)
	mr.HSet("wc:migrations", "20260501T000006Z", "feed")

	down := `version: 1
encoding: json
operations:
  - op: REMOVE_FIELD
    keyspace: users:*
    field: active
`
	m := &applyfetchpb.Migration{
		Id:      "20260501T000006Z",
		UpSql:   yamlAddActive, // forward is YAML → dispatch to rollbackYAML
		DownSql: down,
	}
	if err := a.Rollback(ctx, m); err != nil {
		t.Fatalf("Rollback YAML: %v", err)
	}
	if got, _ := mr.Get("users:1"); strings.Contains(got, "active") {
		t.Errorf("active field should be removed, got %q", got)
	}
	if mr.HGet("wc:migrations", "20260501T000006Z") != "" {
		t.Error("bookkeeping row should be erased on rollback")
	}
	if mr.Exists("wc:data-rollbacks:20260501T000006Z") {
		t.Error("rollback cursor should be cleared on clean run")
	}
}
