package nats_test

import (
	"context"
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
)

// deadDSN points at a port nothing listens on so connect() fails
// fast with connection-refused.
const deadDSN = "nats://127.0.0.1:1"

func deadApplier(t *testing.T) *nats.Applier {
	t.Helper()
	a, err := nats.New(context.Background(), deadDSN)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestAppliedHead_ConnectError pins that an unreachable server
// surfaces as a connect error from AppliedHead.
func TestAppliedHead_ConnectError(t *testing.T) {
	_, err := deadApplier(t).AppliedHead(boundedCtx(t))
	if err == nil || !strings.Contains(err.Error(), "connect") {
		t.Errorf("expected connect error, got %v", err)
	}
}

// TestApply_ReachesConnectError pins that a dispatchable command
// line drives run→runOne→jsContext and surfaces the connect
// failure wrapped as an up_sql error.
func TestApply_ReachesConnectError(t *testing.T) {
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{
		Id:    "ts-1",
		UpSql: "nats kv add wc-migrations",
	})
	if err == nil || !strings.Contains(err.Error(), "up_sql") {
		t.Errorf("expected up_sql connect error, got %v", err)
	}
}

// TestApply_ParseErrorBeforeConnect pins that an unbalanced quote
// is reported by runOne→ParseArgv before any connection attempt.
func TestApply_ParseErrorBeforeConnect(t *testing.T) {
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{
		Id:    "ts-2",
		UpSql: `nats stream edit users --description "unclosed`,
	})
	if err == nil || !strings.Contains(err.Error(), "unbalanced") {
		t.Errorf("expected unbalanced-quote error, got %v", err)
	}
}

// TestApply_CommentAndBareNatsAreNoOps pins two early-return
// branches that need no connection: `#` comment lines are skipped
// by run, and a bare `nats` token reduces to an empty argv that
// runOne treats as a no-op — so Apply succeeds without dialing.
func TestApply_CommentAndBareNatsAreNoOps(t *testing.T) {
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{
		Id:    "ts-3",
		UpSql: "# wc: a comment\n\nnats\n   ",
	})
	if err != nil {
		t.Errorf("comment + bare-nats body should be a no-op, got %v", err)
	}
}

// TestApply_EmptyUpThenPostTxConnect pins that an empty up_sql is
// a no-op and a non-empty up_post_tx is still dispatched (failing
// here against the dead server).
func TestApply_EmptyUpThenPostTxConnect(t *testing.T) {
	err := deadApplier(t).Apply(boundedCtx(t), &applyfetchpb.Migration{
		Id:       "ts-4",
		UpSql:    "",
		UpPostTx: "nats kv add wc-migrations",
	})
	if err == nil || !strings.Contains(err.Error(), "up_post_tx") {
		t.Errorf("expected up_post_tx connect error, got %v", err)
	}
}

// TestRollback_ReachesConnectError pins the down path: down_pre_tx
// is dispatched first and surfaces the connect error.
func TestRollback_ReachesConnectError(t *testing.T) {
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id:        "ts-5",
		DownPreTx: "nats stream rm users",
	})
	if err == nil || !strings.Contains(err.Error(), "down_pre_tx") {
		t.Errorf("expected down_pre_tx connect error, got %v", err)
	}
}

// TestRollback_DownSqlConnectError pins that an empty down_pre_tx
// passes through and the down_sql line surfaces the connect error.
func TestRollback_DownSqlConnectError(t *testing.T) {
	err := deadApplier(t).Rollback(boundedCtx(t), &applyfetchpb.Migration{
		Id:      "ts-6",
		DownSql: "nats kv del wc-migrations ts-6",
	})
	if err == nil || !strings.Contains(err.Error(), "down_sql") {
		t.Errorf("expected down_sql connect error, got %v", err)
	}
}
