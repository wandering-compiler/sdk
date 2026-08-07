package distx_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// fakeDistxServer is a hand-rolled W17DistributedTransactionServer used
// only to exercise the CLIENT helper's metadata round-trip — it does NOT
// touch a real txregistry. On Begin it optionally stamps a w17-conn-id
// into the RESPONSE header (simulating the Rust storage proxy on the
// multi-replica path); a nil connID models the single-replica passthrough
// (no proxy, header absent). It records the INCOMING request metadata of
// every call so the test can assert what the helper attached.
type fakeDistxServer struct {
	distxpb.UnimplementedW17DistributedTransactionServer

	connID string // stamped into the Begin response header; "" = single-replica

	beginMD    metadata.MD
	commitMD   metadata.MD
	rollbackMD metadata.MD
	lastTxID   string
}

func (f *fakeDistxServer) Begin(ctx context.Context, _ *distxpb.BeginRequest) (*distxpb.BeginResponse, error) {
	f.beginMD, _ = metadata.FromIncomingContext(ctx)
	if f.connID != "" {
		// The proxy stamps the routing token into the RESPONSE metadata
		// (a gRPC response header), never the body — body conn_id stays
		// empty, exactly like distx.Server.Begin.
		if err := grpc.SetHeader(ctx, metadata.Pairs(distx.ConnIDHeader, f.connID)); err != nil {
			return nil, err
		}
	}
	return &distxpb.BeginResponse{TxId: "tx-123"}, nil
}

func (f *fakeDistxServer) Commit(ctx context.Context, req *distxpb.CommitRequest) (*distxpb.CommitResponse, error) {
	f.commitMD, _ = metadata.FromIncomingContext(ctx)
	f.lastTxID = req.GetTxId()
	return &distxpb.CommitResponse{}, nil
}

func (f *fakeDistxServer) Rollback(ctx context.Context, req *distxpb.RollbackRequest) (*distxpb.RollbackResponse, error) {
	f.rollbackMD, _ = metadata.FromIncomingContext(ctx)
	f.lastTxID = req.GetTxId()
	return &distxpb.RollbackResponse{}, nil
}

// newFakeClient stands up the fake server on a bufconn listener and
// returns a real gRPC client dialled against it. A real grpc.Server /
// client pair is used (not inprocgrpc) because the test must exercise the
// genuine grpc.Header response-metadata round-trip — grpc.SetHeader on the
// server is only delivered to the client's grpc.Header call option over a
// real transport.
func newFakeClient(t *testing.T, srv *fakeDistxServer) distxpb.W17DistributedTransactionClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	distxpb.RegisterW17DistributedTransactionServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufnet: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return distxpb.NewW17DistributedTransactionClient(conn)
}

// outgoing returns the values of key in ctx's OUTGOING metadata.
func outgoing(t *testing.T, ctx context.Context, key string) []string {
	t.Helper()
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return nil
	}
	return md.Get(key)
}

// TestBegin_MultiReplica proves that when the proxy stamps w17-conn-id
// into the Begin response header, the helper's returned ctx carries BOTH
// w17-tx-id and w17-conn-id outgoing, and Commit carries w17-conn-id too.
func TestBegin_MultiReplica(t *testing.T) {
	srv := &fakeDistxServer{connID: "conn-abc"}
	client := newFakeClient(t, srv)

	tx, ctx, err := distx.Begin(context.Background(), client, &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if got := tx.TxID(); got != "tx-123" {
		t.Errorf("TxID() = %q, want tx-123", got)
	}
	if got := tx.ConnID(); got != "conn-abc" {
		t.Errorf("ConnID() = %q, want conn-abc", got)
	}

	// The derived ctx must carry BOTH routing headers outgoing.
	if got := outgoing(t, ctx, txregistry.HeaderName); len(got) != 1 || got[0] != "tx-123" {
		t.Errorf("outgoing %s = %v, want [tx-123]", txregistry.HeaderName, got)
	}
	if got := outgoing(t, ctx, distx.ConnIDHeader); len(got) != 1 || got[0] != "conn-abc" {
		t.Errorf("outgoing %s = %v, want [conn-abc]", distx.ConnIDHeader, got)
	}

	// Commit from a FRESH ctx — the handle must re-attach conn_id so the
	// proxy can still route it to the pinned replica.
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := srv.commitMD.Get(distx.ConnIDHeader); len(got) != 1 || got[0] != "conn-abc" {
		t.Errorf("Commit incoming %s = %v, want [conn-abc]", distx.ConnIDHeader, got)
	}
	if srv.lastTxID != "tx-123" {
		t.Errorf("Commit body tx_id = %q, want tx-123", srv.lastTxID)
	}
}

// TestRollback_MultiReplica proves Rollback also re-attaches conn_id.
func TestRollback_MultiReplica(t *testing.T) {
	srv := &fakeDistxServer{connID: "conn-xyz"}
	client := newFakeClient(t, srv)

	tx, _, err := distx.Begin(context.Background(), client, &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := srv.rollbackMD.Get(distx.ConnIDHeader); len(got) != 1 || got[0] != "conn-xyz" {
		t.Errorf("Rollback incoming %s = %v, want [conn-xyz]", distx.ConnIDHeader, got)
	}
	if srv.lastTxID != "tx-123" {
		t.Errorf("Rollback body tx_id = %q, want tx-123", srv.lastTxID)
	}
}

// TestBegin_SingleReplica proves the passthrough path: no w17-conn-id in
// the Begin response → the ctx carries ONLY w17-tx-id (byte-for-byte the
// pre-proxy behaviour), and Commit carries no conn_id.
func TestBegin_SingleReplica(t *testing.T) {
	srv := &fakeDistxServer{ /* connID empty → no proxy */ }
	client := newFakeClient(t, srv)

	tx, ctx, err := distx.Begin(context.Background(), client, &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if got := tx.ConnID(); got != "" {
		t.Errorf("ConnID() = %q, want empty", got)
	}

	// Only w17-tx-id, nothing else — strict backward-compat.
	if got := outgoing(t, ctx, txregistry.HeaderName); len(got) != 1 || got[0] != "tx-123" {
		t.Errorf("outgoing %s = %v, want [tx-123]", txregistry.HeaderName, got)
	}
	if got := outgoing(t, ctx, distx.ConnIDHeader); len(got) != 0 {
		t.Errorf("outgoing %s = %v, want none (single-replica)", distx.ConnIDHeader, got)
	}

	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := srv.commitMD.Get(distx.ConnIDHeader); len(got) != 0 {
		t.Errorf("Commit incoming %s = %v, want none (single-replica)", distx.ConnIDHeader, got)
	}
	if got := srv.commitMD.Get(txregistry.HeaderName); len(got) != 0 {
		// The Commit body carries tx_id; the w17-tx-id header is only for
		// storage RPCs, not the distx Commit RPC. Documenting the
		// single-replica Commit metadata shape: no routing headers at all.
		t.Logf("note: Commit incoming carries %s=%v", txregistry.HeaderName, got)
	}
}

// TestConnIDHeader_MatchesRustContract guards the single-source-of-truth
// constant against accidental drift from srcrs/storage-proxy's
// CONN_ID_HEADER.
func TestConnIDHeader_MatchesRustContract(t *testing.T) {
	if distx.ConnIDHeader != "w17-conn-id" {
		t.Fatalf("ConnIDHeader = %q; must equal the Rust proxy CONN_ID_HEADER \"w17-conn-id\"", distx.ConnIDHeader)
	}
}
