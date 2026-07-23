package grpcrollback_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/service/tx/grpcrollback"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// fakeRoller records every Rollback call + lets tests stub the
// returned error.
type fakeRoller struct {
	rolledBack []string
	err        error
}

func (f *fakeRoller) Rollback(txID string) error {
	f.rolledBack = append(f.rolledBack, txID)
	return f.err
}

func ctxWithTxID(id string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("w17-tx-id", id))
}

// Handler error + tx_id metadata → Rollback called with the
// tx_id, original error propagated unchanged.
func TestInterceptor_RollbacksOnError(t *testing.T) {
	roller := &fakeRoller{}
	interceptor := grpcrollback.Interceptor(roller)

	handlerErr := errors.New("boom")
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, handlerErr
	}
	_, err := interceptor(ctxWithTxID("abc-123"), nil, &grpc.UnaryServerInfo{}, handler)
	if !errors.Is(err, handlerErr) {
		t.Errorf("expected handler error propagated; got %v", err)
	}
	if len(roller.rolledBack) != 1 || roller.rolledBack[0] != "abc-123" {
		t.Errorf("expected Rollback(\"abc-123\"); got %v", roller.rolledBack)
	}
}

// Handler success → Rollback NOT called even with tx_id present.
func TestInterceptor_NoRollbackOnSuccess(t *testing.T) {
	roller := &fakeRoller{}
	interceptor := grpcrollback.Interceptor(roller)

	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}
	resp, err := interceptor(ctxWithTxID("abc-123"), nil, &grpc.UnaryServerInfo{}, handler)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp != "ok" {
		t.Errorf("resp = %v, want ok", resp)
	}
	if len(roller.rolledBack) != 0 {
		t.Errorf("Rollback should not fire on success; got %v", roller.rolledBack)
	}
}

// Error + no metadata → no Rollback (caller didn't open a
// distributed tx; nothing to roll back).
func TestInterceptor_NoMetadataNoRollback(t *testing.T) {
	roller := &fakeRoller{}
	interceptor := grpcrollback.Interceptor(roller)

	handler := func(ctx context.Context, req any) (any, error) {
		return nil, errors.New("boom")
	}
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(roller.rolledBack) != 0 {
		t.Errorf("Rollback should not fire without metadata; got %v", roller.rolledBack)
	}
}

// Error + empty tx_id value → no Rollback.
func TestInterceptor_EmptyTxIDNoRollback(t *testing.T) {
	roller := &fakeRoller{}
	interceptor := grpcrollback.Interceptor(roller)

	handler := func(ctx context.Context, req any) (any, error) {
		return nil, errors.New("boom")
	}
	_, err := interceptor(ctxWithTxID(""), nil, &grpc.UnaryServerInfo{}, handler)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(roller.rolledBack) != 0 {
		t.Errorf("Rollback should not fire with empty tx_id; got %v", roller.rolledBack)
	}
}

// Nil roller → interceptor is a pass-through, no panic.
func TestInterceptor_NilRoller(t *testing.T) {
	interceptor := grpcrollback.Interceptor(nil)

	handlerErr := errors.New("boom")
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, handlerErr
	}
	_, err := interceptor(ctxWithTxID("abc-123"), nil, &grpc.UnaryServerInfo{}, handler)
	if !errors.Is(err, handlerErr) {
		t.Errorf("nil roller should pass through; got err = %v", err)
	}
	// No panic — that's the assertion.
}

// Roller's Rollback returns an error (e.g., ErrUnknownTxID
// because the registry was already drained). The interceptor
// must NOT mask the original handler error.
func TestInterceptor_RollbackErrorIgnored(t *testing.T) {
	roller := &fakeRoller{err: txregistry.ErrUnknownTxID}
	interceptor := grpcrollback.Interceptor(roller)

	handlerErr := errors.New("boom")
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, handlerErr
	}
	_, err := interceptor(ctxWithTxID("abc-123"), nil, &grpc.UnaryServerInfo{}, handler)
	if !errors.Is(err, handlerErr) {
		t.Errorf("interceptor must propagate handler error verbatim; got %v want %v", err, handlerErr)
	}
	// Rollback was attempted (stub returned the error).
	if len(roller.rolledBack) != 1 {
		t.Errorf("expected exactly 1 Rollback attempt; got %v", roller.rolledBack)
	}
}

// Integration: real txregistry.Memory + interceptor + handler
// error → registry's Rollback drains the entry, Active() drops.
func TestInterceptor_IntegrationWithMemoryRegistry(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}

	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	txID, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := reg.Active(); got != 1 {
		t.Fatalf("Active after Begin = %d, want 1", got)
	}

	interceptor := grpcrollback.Interceptor(reg)
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, errors.New("handler boom")
	}
	_, err = interceptor(ctxWithTxID(txID), nil, &grpc.UnaryServerInfo{}, handler)
	if err == nil {
		t.Fatal("expected error")
	}

	if got := reg.Active(); got != 0 {
		t.Errorf("Active() = %d after interceptor-driven Rollback, want 0", got)
	}
}
