package redis_test

import (
	"context"
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
)

// deadDSN points at a port nothing listens on so every command
// surfaces a connection-refused error synchronously.
const deadDSN = "redis://127.0.0.1:1"

func deadApplier(t *testing.T) *redis.Applier {
	t.Helper()
	a, err := redis.New(context.Background(), deadDSN)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestAppliedHead_ConnectionRefused pins that HKEYS against an
// unreachable server surfaces as an error (not a silent empty
// head).
func TestAppliedHead_ConnectionRefused(t *testing.T) {
	_, err := deadApplier(t).AppliedHead(boundedCtx(t))
	if err == nil || !strings.Contains(err.Error(), "HKEYS") {
		t.Errorf("expected HKEYS connection error, got %v", err)
	}
}

// TestApply_CommandBodyConnectionRefused pins that a plain
// command body dispatches through run→do and surfaces the exec
// error with the offending line.
func TestApply_CommandBodyConnectionRefused(t *testing.T) {
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{
		Id:    "ts-1",
		UpSql: "HSET wc:migrations ts-1 deadbeef",
	})
	if err == nil || !strings.Contains(err.Error(), "up_sql") {
		t.Errorf("expected up_sql exec error, got %v", err)
	}
}

// TestApply_CommentOnlyUpThenPostTx pins two branches at once:
// an all-comment up_sql is a no-op (FilterComments empties it),
// and a non-empty up_post_tx is still dispatched (and here fails
// against the dead server).
func TestApply_CommentOnlyUpThenPostTx(t *testing.T) {
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{
		Id:       "ts-2",
		UpSql:    "# wc: only a comment\n\n",
		UpPostTx: "HSET wc:migrations ts-2 cafe",
	})
	if err == nil || !strings.Contains(err.Error(), "up_post_tx") {
		t.Errorf("expected up_post_tx error, got %v", err)
	}
}

// TestRollback_CommandBodyConnectionRefused pins the down path
// runs down_pre_tx then down_sql and surfaces the error.
func TestRollback_CommandBodyConnectionRefused(t *testing.T) {
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id:      "ts-3",
		DownSql: "HDEL wc:migrations ts-3",
	})
	if err == nil || !strings.Contains(err.Error(), "down_sql") {
		t.Errorf("expected down_sql error, got %v", err)
	}
}

const yamlAddBody = `version: 1
encoding: json
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: users:*
    field: active
    value: true
`

// TestApply_YAMLReachesCursorThenFails pins the YAML dispatch
// path: parse + codec + transform-VM build all succeed, then the
// first cursor read (HEXISTS) hits the dead server.
func TestApply_YAMLReachesCursorThenFails(t *testing.T) {
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{
		Id:    "ts-yaml",
		UpSql: yamlAddBody,
	})
	if err == nil || !strings.Contains(err.Error(), "cursor read") {
		t.Errorf("expected cursor-read connection error, got %v", err)
	}
}

// TestApply_YAMLProtobufCodecError pins that a protobuf-encoded
// body with a non-FDS descriptor is refused at buildCodec —
// before any network call.
func TestApply_YAMLProtobufCodecError(t *testing.T) {
	body := `version: 1
encoding: protobuf
proto_descriptor: aGVsbG8=
proto_message: pkg.User
operations:
  - op: ADD_FIELD_DEFAULT
    keyspace: x:*
    field: y
    value: 1
`
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{Id: "ts-pb", UpSql: body})
	if err == nil || strings.Contains(err.Error(), "cursor") {
		t.Errorf("expected codec error before network, got %v", err)
	}
}

// TestApply_YAMLTransformCompileError pins that a TRANSFORM_FIELD
// op with an invalid script aborts at buildTransformVMs before
// any network call.
func TestApply_YAMLTransformCompileError(t *testing.T) {
	body := `version: 1
encoding: json
operations:
  - op: TRANSFORM_FIELD
    keyspace: x:*
    script_lang: starlark
    script: |
      x = 1
`
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{Id: "ts-tf", UpSql: body})
	if err == nil || strings.Contains(err.Error(), "cursor") {
		t.Errorf("expected transform compile error before network, got %v", err)
	}
}

// TestApply_YAMLMalformedParseError pins that a body that looks
// like YAML but doesn't parse is rejected at Unmarshal.
func TestApply_YAMLMalformedParseError(t *testing.T) {
	body := "version: 1\noperations: [unterminated\n"
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{Id: "ts-bad", UpSql: body})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected YAML parse error, got %v", err)
	}
}

// TestRollback_YAMLIrreversibleRefused pins that an irreversible
// down body is refused without touching the network.
func TestRollback_YAMLIrreversibleRefused(t *testing.T) {
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id:      "ts-irr",
		UpSql:   yamlAddBody,
		DownSql: "# wc:irreversible: REMOVE_FIELD has no inverse",
	})
	if err == nil || !strings.Contains(err.Error(), "irreversible") {
		t.Errorf("expected irreversible refusal, got %v", err)
	}
}

// TestRollback_YAMLMalformedDownParseError pins the down-body
// parse error branch of rollbackYAMLDataMigration.
func TestRollback_YAMLMalformedDownParseError(t *testing.T) {
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id:      "ts-baddown",
		UpSql:   yamlAddBody,
		DownSql: "version: 1\noperations: [unterminated\n",
	})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected down-body parse error, got %v", err)
	}
}

// TestRollback_YAMLReachesCursorThenFails pins that a valid YAML
// down body runs the rollback machinery up to the first cursor
// read against the dead server.
func TestRollback_YAMLReachesCursorThenFails(t *testing.T) {
	down := `version: 1
encoding: json
operations:
  - op: REMOVE_FIELD
    keyspace: users:*
    field: active
`
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id:      "ts-rbcur",
		UpSql:   yamlAddBody,
		DownSql: down,
	})
	if err == nil || !strings.Contains(err.Error(), "cursor read") {
		t.Errorf("expected rollback cursor-read error, got %v", err)
	}
}

// TestApply_CommandBodyUnbalancedQuote pins the ParseArgv-error
// branch of run: an unbalanced quote in a command line surfaces
// as a parse error before any dispatch.
func TestApply_CommandBodyUnbalancedQuote(t *testing.T) {
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{
		Id:    "ts-q",
		UpSql: `EVAL "unclosed`,
	})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("expected parse error, got %v", err)
	}
}

// TestSetParallelOverride_NoPanic pins the operator override
// setter is a plain field write (exercised here for coverage; its
// effect is only observable on the live data-migration path).
func TestSetParallelOverride_NoPanic(t *testing.T) {
	deadApplier(t).SetParallelOverride(8)
}

// TestRollback_DownPreTxConnectionRefused pins the down_pre_tx run error
// arm (the existing rollback test only sets down_sql).
func TestRollback_DownPreTxConnectionRefused(t *testing.T) {
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id:        "ts-pre",
		DownPreTx: "HDEL wc:migrations ts-pre",
		DownSql:   "HDEL wc:migrations ts-pre",
	})
	if err == nil || !strings.Contains(err.Error(), "down_pre_tx") {
		t.Errorf("expected down_pre_tx error, got %v", err)
	}
}
