package distx_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// A TxHandle drives one Commit or one Rollback. A second call used to reach
// the server, which no longer held the id, and come back NotFound — the same
// answer a LOST transaction gets. It now says what actually happened.
func TestTxHandle_SecondFinishSaysTheTransactionAlreadyEnded(t *testing.T) {
	client, reg := newClientServer(t)

	h, _, err := distx.Begin(context.Background(), client, &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := h.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for name, finish := range map[string]func(context.Context) error{"Commit": h.Commit, "Rollback": h.Rollback} {
		err := finish(context.Background())
		if !errors.Is(err, distx.ErrHandleFinished) || !strings.Contains(err.Error(), "committed") {
			t.Errorf("second %s = %v, want ErrHandleFinished saying it was committed", name, err)
		}
		if status.Code(err) == codes.NotFound {
			t.Errorf("second %s reached the server: %v", name, err)
		}
	}

	h2, _, err := distx.Begin(context.Background(), client, &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := h2.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := h2.Commit(context.Background()); !errors.Is(err, distx.ErrHandleFinished) || !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("Commit after Rollback = %v, want ErrHandleFinished saying it was rolled back", err)
	}
	if reg.Active() != 0 {
		t.Errorf("Active() = %d, want 0", reg.Active())
	}
}

// A transaction a failed participant doomed while a sibling still held it:
// the coordinator's Commit is Aborted (not retryable, not NotFound), and its
// own Rollback succeeds — the rollback is already decided.
func TestServer_DoomedTransaction_CommitAbortsAndRollbackSucceeds(t *testing.T) {
	_, reg := newClientServer(t)
	srv := distx.NewServer(reg)

	id, err := reg.Begin(context.Background(), txregistry.BeginOptions{ConnectionName: "main"})
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := reg.LookupTx(context.Background(), id, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reg.RollbackCaused(done, id, "a sibling call failed"); !errors.Is(err, txregistry.ErrTxDoomed) {
		t.Fatalf("RollbackCaused = %v, want ErrTxDoomed", err)
	}

	_, err = srv.Commit(context.Background(), &distxpb.CommitRequest{TxId: id})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("Commit of a doomed transaction = %v, want Aborted", err)
	}
	if _, err := srv.Rollback(done, &distxpb.RollbackRequest{TxId: id}); err != nil {
		t.Fatalf("Rollback of a doomed transaction = %v, want success (the rollback is already decided)", err)
	}
	release()
	if reg.Active() != 0 {
		t.Fatalf("Active() = %d after the holder released, want 0", reg.Active())
	}
}

// A finish the coordinator answered with a failure does not use the handle
// up: a Commit refused because a call still holds the transaction
// (ErrTxBusy, the transaction intact) is retried on the same handle and
// commits.
func TestTxHandle_RefusedFinishCanBeRetried(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db}, txregistry.WithFinishWait(50*time.Millisecond))
	srv := grpc.NewServer()
	distxpb.RegisterW17DistributedTransactionServer(srv, distx.NewServer(reg))
	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough://bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := distxpb.NewW17DistributedTransactionClient(conn)

	h, _, err := distx.Begin(context.Background(), client, &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, release, err := reg.LookupTx(context.Background(), h.TxID(), "main")
	if err != nil {
		t.Fatalf("LookupTx: %v", err)
	}
	err = h.Commit(context.Background())
	if status.Code(err) != codes.FailedPrecondition {
		release()
		t.Fatalf("Commit while a call holds the transaction = %v, want FailedPrecondition (busy)", err)
	}
	release()
	if err := h.Commit(context.Background()); err != nil {
		t.Fatalf("Commit retried on the same handle = %v, want it to commit", err)
	}
	if reg.Active() != 0 {
		t.Errorf("Active() = %d after the retried Commit, want 0", reg.Active())
	}
}
