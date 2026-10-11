package grpcrollback_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/service/tx/grpcrollback"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// A coordinator fans two calls out into one transaction. One fails while the
// other still holds the transaction's lease. The failed call must discard the
// transaction whatever the state of its own context — the coordinator's
// later Commit must fail, not commit over a failed participant.

func doomFixture(t *testing.T, opts ...txregistry.MemoryOption) (*txregistry.Memory, *sql.DB, string, txregistry.ReleaseFunc) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db}, opts...)
	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// The sibling: adopted, wrote, still running.
	tx, release, err := reg.LookupTx(context.Background(), id, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	return reg, db, id, release
}

func failingCall(ctx context.Context, reg *txregistry.Memory) error {
	_, err := grpcrollback.Interceptor(reg)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/app.Orders/Reserve"},
		func(context.Context, any) (any, error) { return nil, status.Error(codes.DeadlineExceeded, "timed out") })
	return err
}

// The failing call's context is already done when the interceptor runs.
func TestInterceptor_DoneContextBehindASiblingStillDiscardsTheTransaction(t *testing.T) {
	reg, db, id, release := doomFixture(t, txregistry.WithFinishWait(time.Minute))

	ctx, cancel := context.WithCancel(metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(txregistry.HeaderName, id)))
	cancel()

	returned := make(chan error, 1)
	go func() { returned <- failingCall(ctx, reg) }()
	// The sibling finishes a little later; the rollback must still happen.
	time.Sleep(50 * time.Millisecond)
	release()
	if err := <-returned; status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("handler error not propagated: %v", err)
	}

	if err := reg.Commit(context.Background(), id); err == nil {
		t.Fatal("Commit succeeded for a transaction with a failed participant")
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows committed, want 0", n)
	}
}

// No finish-wait at all: the rollback cannot run before the interceptor
// returns, so the transaction is doomed, the doom is reported, and the
// rollback runs when the sibling releases.
func TestInterceptor_ZeroFinishWaitDoomsTheTransaction(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	reg, db, id, release := doomFixture(t, txregistry.WithFinishWait(0))
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(txregistry.HeaderName, id))
	if err := failingCall(ctx, reg); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("handler error not propagated: %v", err)
	}
	if !strings.Contains(logged.String(), "doomed") {
		t.Errorf("a deferred rollback was not reported; log = %q", logged.String())
	}

	if err := reg.Commit(context.Background(), id); !errors.Is(err, txregistry.ErrTxDoomed) {
		t.Fatalf("Commit = %v, want ErrTxDoomed — a transaction with a failed participant must not commit", err)
	}
	release()
	if reg.Active() != 0 {
		t.Fatalf("Active() = %d after the sibling released, want 0", reg.Active())
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows committed, want 0", n)
	}
}
