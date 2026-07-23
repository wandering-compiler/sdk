package distx_test

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// TestClientBegin_TransportError covers distx.Begin's client-error arm: a
// cancelled context makes the underlying gRPC Begin fail, and Begin returns the
// original ctx unchanged alongside the error.
func TestClientBegin_TransportError(t *testing.T) {
	client, _ := newClientServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, gotCtx, err := distx.Begin(ctx, client, &distxpb.BeginRequest{ConnectionName: "main"})
	if err == nil {
		t.Fatal("cancelled context should fail Begin")
	}
	if gotCtx != ctx {
		t.Error("on error Begin should return the original ctx unchanged")
	}
}

// TestServerBegin_InternalError covers Server.Begin's Internal arm: a registry
// backed by a closed DB fails at BeginTx with a non-ErrUnknownConnection error.
func TestServerBegin_InternalError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	srv := distx.NewServer(txregistry.NewMemory(map[string]*sql.DB{"main": db}))
	if _, err := srv.Begin(context.Background(), &distxpb.BeginRequest{ConnectionName: "main"}); err == nil {
		t.Error("Begin on a closed-DB registry should return an Internal error")
	}
}

// TestServerCommit_InternalError covers Server.Commit's Internal arm: finalising
// the underlying tx out-of-band makes the registry Commit return ErrTxDone
// (mapped to Internal, distinct from the NotFound ErrUnknownTxID arm).
func TestServerCommit_InternalError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := txregistry.NewMemory(map[string]*sql.DB{"main": db})
	srv := distx.NewServer(reg)

	resp, err := srv.Begin(context.Background(), &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx, err := reg.LookupTx(resp.GetTxId(), "main")
	if err != nil {
		t.Fatalf("LookupTx: %v", err)
	}
	if err := tx.Rollback(); err != nil { // finalise behind the registry's back
		t.Fatalf("direct rollback: %v", err)
	}
	if _, err := srv.Commit(context.Background(), &distxpb.CommitRequest{TxId: resp.GetTxId()}); err == nil {
		t.Error("Commit of an already-finalised tx should return an Internal error")
	}
}
