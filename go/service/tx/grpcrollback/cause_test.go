package grpcrollback_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/service/tx/grpcrollback"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// causingRoller implements the optional CausedRoller slice and records
// what it was told.
type causingRoller struct {
	fakeRoller
	causes []string
}

func (c *causingRoller) RollbackCaused(ctx context.Context, txID, cause string) error {
	c.causes = append(c.causes, cause)
	return c.Rollback(ctx, txID)
}

// A roller that can hold a cause is given one, and the cause names the
// method and the status code that triggered the discard — the two facts
// the coordinator cannot reconstruct from its own Commit failure.
func TestInterceptor_PassesCauseWhenRollerAcceptsOne(t *testing.T) {
	roller := &causingRoller{}
	interceptor := grpcrollback.Interceptor(roller)

	handler := func(ctx context.Context, req any) (any, error) {
		return nil, status.Error(codes.NotFound, "no rows")
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/docs.DocumentMutation/UnmarkPaid"}
	if _, err := interceptor(ctxWithTxID("abc-123"), nil, info, handler); err == nil {
		t.Fatal("expected the handler error to propagate")
	}
	if len(roller.rolledBack) != 1 || roller.rolledBack[0] != "abc-123" {
		t.Fatalf("expected the rollback to still happen; got %v", roller.rolledBack)
	}
	if len(roller.causes) != 1 {
		t.Fatalf("expected exactly one cause recorded; got %v", roller.causes)
	}
	cause := roller.causes[0]
	if !strings.Contains(cause, "/docs.DocumentMutation/UnmarkPaid") {
		t.Errorf("cause = %q, want it to name the failing method", cause)
	}
	if !strings.Contains(cause, "NotFound") {
		t.Errorf("cause = %q, want it to name the status code", cause)
	}
	// The handler's own message has not been through the error layer's
	// sanitisation, so it must not ride along into a diagnostic that a
	// client can read.
	if strings.Contains(cause, "no rows") {
		t.Errorf("cause = %q, want the raw handler message left out", cause)
	}
}

// The whole chain over the REAL registry, because the two halves
// passing in isolation does not say they are connected: the optional
// slice is satisfied by name, so a drift in the method's signature
// would silently drop the interceptor back to the plain rollback and
// every other test here would stay green.
//
// This is the consumer's exact sequence — a call inside the
// transaction fails, its caller tolerates the error, and the Commit
// that follows has to explain itself.
func TestInterceptor_EndToEnd_CommitNamesTheFailedCall(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})

	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	interceptor := grpcrollback.Interceptor(reg)
	handler := func(ctx context.Context, req any) (any, error) {
		// A guarded write whose WHERE matched nothing.
		return nil, status.Error(codes.NotFound, "no rows")
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/docs.DocumentMutation/UnmarkPaid"}
	if _, err := interceptor(ctxWithTxID(id), nil, info, handler); status.Code(err) != codes.NotFound {
		t.Fatalf("interceptor = %v, want the handler's NotFound propagated", err)
	}

	// The caller tolerated that NotFound and went on to commit.
	err = reg.Commit(context.Background(), id)
	if !errors.Is(err, txregistry.ErrUnknownTxID) {
		t.Fatalf("Commit = %v, want ErrUnknownTxID", err)
	}
	if !strings.Contains(err.Error(), "/docs.DocumentMutation/UnmarkPaid") {
		t.Errorf("Commit error = %q, want it to name the call that discarded the transaction", err)
	}
}

// A roller that predates the optional slice keeps working: the
// transaction is still discarded, only the diagnostic is lost.
func TestInterceptor_PlainRollerStillRollsBack(t *testing.T) {
	roller := &fakeRoller{}
	interceptor := grpcrollback.Interceptor(roller)

	handler := func(ctx context.Context, req any) (any, error) {
		return nil, status.Error(codes.Internal, "boom")
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/docs.DocumentMutation/UnmarkPaid"}
	if _, err := interceptor(ctxWithTxID("abc-123"), nil, info, handler); err == nil {
		t.Fatal("expected the handler error to propagate")
	}
	if len(roller.rolledBack) != 1 || roller.rolledBack[0] != "abc-123" {
		t.Errorf("expected Rollback(\"abc-123\"); got %v", roller.rolledBack)
	}
}
