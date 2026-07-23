package distx_test

import (
	"context"
	"database/sql"
	"net"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// newClientServer wires a bufconn-backed gRPC server holding a
// registry with two connections ("main" + "audit") so tests can
// exercise both single-conn happy paths and the multi-conn
// connection-routing semantics M2-D added.
func newClientServer(t *testing.T, opts ...distx.Option) (distxpb.W17DistributedTransactionClient, *txregistry.Memory) {
	t.Helper()
	dbMain, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite main: %v", err)
	}
	t.Cleanup(func() { _ = dbMain.Close() })
	dbAudit, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite audit: %v", err)
	}
	t.Cleanup(func() { _ = dbAudit.Close() })

	reg := txregistry.NewMemory(map[string]*sql.DB{
		"main":  dbMain,
		"audit": dbAudit,
	})
	srv := grpc.NewServer()
	distxpb.RegisterW17DistributedTransactionServer(srv, distx.NewServer(reg, opts...))

	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough://bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return distxpb.NewW17DistributedTransactionClient(conn), reg
}

// Begin returns a tx_id and the registry holds the tx; conn_id
// stays empty per single-instance default.
func TestServer_Begin_RegistersTx(t *testing.T) {
	cl, reg := newClientServer(t)

	resp, err := cl.Begin(context.Background(), &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if resp.GetTxId() == "" {
		t.Error("expected non-empty tx_id")
	}
	if resp.GetConnId() != "" {
		t.Errorf("single-instance Begin should leave conn_id empty; got %q", resp.GetConnId())
	}
	if _, err := reg.LookupTx(resp.GetTxId(), "main"); err != nil {
		t.Errorf("registry should hold the freshly-begun tx; got err %v", err)
	}
	// Cleanup so Memory doesn't leak the open tx.
	_, _ = cl.Rollback(context.Background(), &distxpb.RollbackRequest{TxId: resp.GetTxId()})
}

// Begin against an unknown connection name maps to
// `codes.InvalidArgument` — caller passed a name the binary
// doesn't host.
func TestServer_Begin_UnknownConnection_InvalidArgument(t *testing.T) {
	cl, _ := newClientServer(t)
	_, err := cl.Begin(context.Background(), &distxpb.BeginRequest{ConnectionName: "nope"})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("Begin(nope): code = %s, want InvalidArgument", got)
	}
}

// Commit closes the tx; subsequent registry lookup misses.
func TestServer_Commit_ClosesTx(t *testing.T) {
	cl, reg := newClientServer(t)

	begin, err := cl.Begin(context.Background(), &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := cl.Commit(context.Background(), &distxpb.CommitRequest{TxId: begin.GetTxId()}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := reg.LookupTx(begin.GetTxId(), "main"); err == nil {
		t.Error("registry should not hold the tx after Commit")
	}
}

// Rollback similar — registry drops the entry.
func TestServer_Rollback_DropsTx(t *testing.T) {
	cl, reg := newClientServer(t)

	begin, err := cl.Begin(context.Background(), &distxpb.BeginRequest{ConnectionName: "main"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := cl.Rollback(context.Background(), &distxpb.RollbackRequest{TxId: begin.GetTxId()}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := reg.LookupTx(begin.GetTxId(), "main"); err == nil {
		t.Error("registry should not hold the tx after Rollback")
	}
}

// Commit / Rollback on an unknown id surface as NotFound.
func TestServer_UnknownID_NotFound(t *testing.T) {
	cl, _ := newClientServer(t)

	_, err := cl.Commit(context.Background(), &distxpb.CommitRequest{TxId: "does-not-exist"})
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("Commit(unknown): code = %s, want NotFound", got)
	}
	_, err = cl.Rollback(context.Background(), &distxpb.RollbackRequest{TxId: "does-not-exist"})
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("Rollback(unknown): code = %s, want NotFound", got)
	}
}

// NewServer(nil) panics — defensive.
func TestNewServer_NilRegistry_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil registry")
		}
	}()
	_ = distx.NewServer(nil)
}

// Tier 2 (slice 6Q-F): Begin's tx_timeout_ms threads through
// to the registry's WithTimeout wrap. After the deadline, the
// underlying tx auto-rolls back AND the slice 6Q-F follow-up
// background-watcher drains the registry entry → subsequent
// Commit surfaces NotFound (the tx is gone, not just a failed
// Commit attempt — clearer UX for the caller).
func TestServer_Begin_TxTimeoutMs_AutoRollback(t *testing.T) {
	cl, _ := newClientServer(t)

	begin, err := cl.Begin(context.Background(), &distxpb.BeginRequest{
		ConnectionName: "main",
		TxTimeoutMs:    50,
	})
	if err != nil {
		t.Fatalf("Begin with tx_timeout_ms=50: %v", err)
	}
	// Sleep past the deadline so the WithTimeout fires, the
	// sql package auto-rollbacks, AND the watcher drains the
	// registry entry.
	time.Sleep(200 * time.Millisecond)

	_, err = cl.Commit(context.Background(), &distxpb.CommitRequest{TxId: begin.GetTxId()})
	if err == nil {
		t.Fatal("expected Commit error after timeout-driven auto-rollback + watcher drain; got nil")
	}
	// NotFound — the watcher drained the entry; the gRPC
	// server maps txregistry.ErrUnknownTxID onto codes.NotFound.
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("Commit after timeout + watcher: code = %s, want NotFound", got)
	}
}

// Negative tx_timeout_ms rejects at the gRPC boundary —
// matches Method.timeout_ms's negative-rejection rule (slice
// 6Q-F Tier 1).
func TestServer_Begin_NegativeTxTimeoutMs_Rejected(t *testing.T) {
	cl, _ := newClientServer(t)
	_, err := cl.Begin(context.Background(), &distxpb.BeginRequest{
		ConnectionName: "main",
		TxTimeoutMs:    -1,
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("Begin with negative tx_timeout_ms: code = %s, want InvalidArgument", got)
	}
}

// G3-DT-01: a Begin with `tx_timeout_ms = 0` (the proto3
// default — typical for callers that don't set the field)
// inherits the server's [DefaultOrphanTimeout] fallback. The
// registry's Tier-2 watcher drains the entry when the
// fallback fires, so a caller crash between Begin and Commit
// no longer leaks the *sql.Tx forever.
func TestServer_Begin_DefaultTxTimeout_OrphanCleanup(t *testing.T) {
	// Override the lib default to a fast-firing value so the
	// test takes ~200ms, not 5 minutes.
	cl, reg := newClientServer(t, distx.WithDefaultTxTimeout(50*time.Millisecond))

	begin, err := cl.Begin(context.Background(), &distxpb.BeginRequest{
		ConnectionName: "main",
		// TxTimeoutMs intentionally omitted (proto3 default 0).
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if reg.Active() != 1 {
		t.Fatalf("expected 1 active tx after Begin; got %d", reg.Active())
	}
	// Wait past the default fallback. Watcher drains the entry,
	// underlying sql package auto-rollbacks.
	time.Sleep(200 * time.Millisecond)

	if reg.Active() != 0 {
		t.Errorf("registry should be drained after default-timeout fired; Active=%d", reg.Active())
	}
	// Commit on the now-drained entry surfaces NotFound (the
	// caller learns the tx is gone, not a vague Commit failure).
	_, err = cl.Commit(context.Background(), &distxpb.CommitRequest{TxId: begin.GetTxId()})
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("Commit after default timeout: code = %s, want NotFound", got)
	}
}

// Operators can opt out of the default fallback with
// [distx.WithDefaultTxTimeout(0)] — tx with no caller-supplied
// timeout then registers with no cleanup mechanism (the
// pre-G3-DT-01 behaviour). Documented escape hatch for
// deployments where every caller is trusted to drive
// Commit/Rollback themselves.
func TestServer_Begin_DefaultTxTimeout_OptOut(t *testing.T) {
	cl, reg := newClientServer(t, distx.WithDefaultTxTimeout(0))

	begin, err := cl.Begin(context.Background(), &distxpb.BeginRequest{
		ConnectionName: "main",
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// No timeout watcher → entry stays past any reasonable
	// wait (we sleep briefly to confirm).
	time.Sleep(100 * time.Millisecond)
	if reg.Active() != 1 {
		t.Errorf("opt-out should leave entry resident; Active=%d", reg.Active())
	}
	// Manual cleanup so the test doesn't leak across runs.
	_, _ = cl.Rollback(context.Background(), &distxpb.RollbackRequest{TxId: begin.GetTxId()})
}

// Caller-supplied tx_timeout_ms wins over the server's
// default — matches the "explicit beats implicit" rule. Test
// sets a long server default + a short caller bound; the short
// one must fire first.
func TestServer_Begin_CallerTimeoutOverridesDefault(t *testing.T) {
	cl, reg := newClientServer(t, distx.WithDefaultTxTimeout(10*time.Minute))

	_, err := cl.Begin(context.Background(), &distxpb.BeginRequest{
		ConnectionName: "main",
		TxTimeoutMs:    50, // caller's bound is 50ms
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if reg.Active() != 0 {
		t.Errorf("caller's 50ms should have drained the entry; Active=%d", reg.Active())
	}
}
