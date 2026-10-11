package distx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
