package distx

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// ConnIDHeader is the gRPC metadata header carrying the storage
// proxy's routing token. The Rust storage proxy (srcrs/storage-
// proxy, see CONN_ID_HEADER in src/router.rs) mints an opaque
// conn_id on Begin and stamps it into the Begin RESPONSE metadata;
// the client must echo it back as a REQUEST metadata header on every
// subsequent storage RPC of the transaction (and on Commit/Rollback)
// so the proxy routes to the replica that opened the tx.
//
// Empty / absent in single-replica deployments (no proxy in front of
// storage) — the helper then behaves byte-for-byte like the bare
// w17-tx-id threading the facades did before the proxy existed.
//
// Single source of truth on the Go side; it MUST stay equal to the
// Rust router's CONN_ID_HEADER constant.
const ConnIDHeader = "w17-conn-id"

// TxHandle drives a single distributed transaction opened via [Begin].
// It carries the proxy routing token (conn_id, possibly empty) and the
// tx_id, and re-attaches both onto the ctx of Commit / Rollback so the
// proxy can route the finishing RPC to the pinned replica even when the
// caller passes a fresh context.
//
// A TxHandle is single-use: drive exactly one Commit OR one Rollback.
type TxHandle struct {
	client distxpb.W17DistributedTransactionClient
	txID   string
	connID string
}

// Begin opens a distributed transaction through client and returns a
// [TxHandle] plus a derived context already carrying the outgoing
// metadata every storage RPC of this transaction must echo:
//
//   - w17-tx-id  — the tx adoption header (always present), and
//   - w17-conn-id — the proxy routing token (only when the storage
//     proxy supplied one in the Begin response metadata; absent in
//     single-replica deployments).
//
// Pass the returned ctx to every storage RPC that must run inside this
// transaction, then finish with [TxHandle.Commit] or
// [TxHandle.Rollback].
//
// Backward-compat: when no proxy sits in front of storage the Begin
// response carries no w17-conn-id, so the returned ctx carries ONLY
// w17-tx-id — identical to the pre-proxy hand-rolled
// metadata.AppendToOutgoingContext(ctx, "w17-tx-id", txID) call. No
// added metadata, no behavioural change for single-replica /
// single-binary deployments.
func Begin(
	ctx context.Context,
	client distxpb.W17DistributedTransactionClient,
	req *distxpb.BeginRequest,
) (*TxHandle, context.Context, error) {
	var header metadata.MD
	resp, err := client.Begin(ctx, req, grpc.Header(&header))
	if err != nil {
		return nil, ctx, err
	}

	connID := firstNonEmpty(header.Get(ConnIDHeader))
	h := &TxHandle{client: client, txID: resp.GetTxId(), connID: connID}
	return h, h.attach(ctx), nil
}

// Commit finalises the transaction. The conn_id (when the proxy
// supplied one) is re-attached to ctx as outgoing metadata so the
// proxy routes the Commit to the pinned replica before evicting its
// affinity entry; the tx_id travels in the request body as before.
//
// ctx may be a fresh context (the handle re-attaches its own routing
// metadata); whatever the caller passes is preserved and only the
// w17-conn-id header is added on top (and only when non-empty).
func (h *TxHandle) Commit(ctx context.Context) error {
	_, err := h.client.Commit(h.attach(ctx), &distxpb.CommitRequest{TxId: h.txID})
	return err
}

// Rollback discards the transaction. Like [TxHandle.Commit], it
// re-attaches the conn_id so the proxy routes the Rollback to the
// pinned replica before evicting.
func (h *TxHandle) Rollback(ctx context.Context) error {
	_, err := h.client.Rollback(h.attach(ctx), &distxpb.RollbackRequest{TxId: h.txID})
	return err
}

// TxID returns the transaction id the caller threads as the w17-tx-id
// header (already attached on the ctx [Begin] returned).
func (h *TxHandle) TxID() string { return h.txID }

// ConnID returns the proxy routing token, or "" in single-replica
// deployments where no proxy minted one.
func (h *TxHandle) ConnID() string { return h.connID }

// attach derives a ctx carrying this tx's outgoing routing metadata.
// w17-tx-id is always attached; w17-conn-id is attached only when the
// proxy supplied one — so single-replica output is byte-for-byte the
// bare w17-tx-id threading. metadata.AppendToOutgoingContext is
// additive, so any metadata the caller already set on ctx is kept.
func (h *TxHandle) attach(ctx context.Context) context.Context {
	kv := []string{txregistry.HeaderName, h.txID}
	if h.connID != "" {
		kv = append(kv, ConnIDHeader, h.connID)
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}

// firstNonEmpty returns the first non-empty value, or "" — the proxy
// stamps a single w17-conn-id, but a metadata key can technically hold
// several values; we take the first meaningful one.
func firstNonEmpty(vals []string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
