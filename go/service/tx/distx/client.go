package distx

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/core/observx"
	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
	"github.com/wandering-compiler/sdk/go/service/tx/txscope"
)

// ErrRollbackOnly is what committing a transaction returns after a JOINED
// (nested) part of it rolled back. The root rolls the whole transaction back
// instead: without savepoints that part's writes cannot be undone on their
// own, and committing would keep a half-done write the inner method refused.
var ErrRollbackOnly = errors.New("distx: a nested part of this transaction rolled back, so the transaction was rolled back instead of committed")

// ErrNestedMismatch is returned by a [Begin] that finds a transaction already
// open on ctx but cannot JOIN it: a different connection, or a different
// isolation level asked for explicitly.
//
// Refused rather than opened as a second, independent transaction. A second
// transaction on ANOTHER connection is not atomic with the first, which is
// exactly what nesting a call inside a transaction promises its reader; one on
// the SAME database can deadlock against the outer transaction's own locks.
// Isolation cannot change on a running transaction, so honouring the inner
// request is impossible and ignoring it is a silent downgrade.
var ErrNestedMismatch = errors.New("distx: cannot join the transaction already open on this context")

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

	// connection and isolation are what this transaction was opened with —
	// what a nested Begin must match to join it.
	connection string
	isolation  distxpb.Isolation

	// scope is the in-process face of this transaction (txscope): what runs
	// on commit, and whether a nested part rolled back.
	scope *txscope.Scope
	// joined marks a handle for a Begin that found a transaction already
	// open on ctx and JOINED it. Its Commit is a no-op (the root commits)
	// and its Rollback marks the root rollback-only.
	joined bool
}

// rootKey carries the ROOT handle on a transaction's context, so a nested
// Begin can join it with the root's own routing — see joinCtx.
type rootKey struct{}

func rootFrom(ctx context.Context) *TxHandle {
	h, _ := ctx.Value(rootKey{}).(*TxHandle)
	return h
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
// w17-tx-id — the same headers the pre-proxy hand-rolled threading sent.
//
// NESTED: when ctx already belongs to an open transaction — a business method
// composing another one inside its own transaction — Begin JOINS it: no RPC, a
// handle whose Commit leaves committing to the root and whose Rollback marks
// the root rollback-only. A second Begin used to append a second `w17-tx-id`;
// storage adopts the FIRST, so the inner writes landed in the outer
// transaction while the inner "committed" an empty one of its own.
//
// A join requires the SAME connection, and an isolation that is either
// unspecified or the root's; anything else is [ErrNestedMismatch]. The inner
// request's tx_timeout_ms is not applied — the root's bound governs the one
// transaction there is.
func Begin(
	ctx context.Context,
	client distxpb.W17DistributedTransactionClient,
	req *distxpb.BeginRequest,
) (*TxHandle, context.Context, error) {
	if root := rootFrom(ctx); root != nil && txscope.Active(ctx) {
		return join(ctx, root, req)
	}
	var header metadata.MD
	resp, err := client.Begin(ctx, req, grpc.Header(&header))
	if err != nil {
		return nil, ctx, err
	}

	scoped, scope := txscope.New(ctx)
	h := &TxHandle{
		client:     client,
		txID:       resp.GetTxId(),
		connID:     firstNonEmpty(header.Get(ConnIDHeader)),
		connection: req.GetConnectionName(),
		isolation:  req.GetIsolation(),
		scope:      scope,
	}
	return h, h.attach(context.WithValue(scoped, rootKey{}, h)), nil
}

// join returns the joined handle for a Begin inside root's transaction.
//
// The returned ctx carries the ROOT's routing headers, set — not appended —
// even when ctx already shows them. A composed binary's in-process hop
// (inprocgrpc) moves the caller's outgoing metadata to the INCOMING side and
// clears the outgoing one, while ctx values — the scope and this root — come
// through. Joining on whatever outgoing metadata happened to be left would
// then send the inner writes with no `w17-tx-id` at all: each one committed
// on its own, outside the transaction its method believes it is in.
func join(ctx context.Context, root *TxHandle, req *distxpb.BeginRequest) (*TxHandle, context.Context, error) {
	if req.GetConnectionName() != root.connection {
		return nil, ctx, fmt.Errorf("%w: it is on connection %q and this Begin asks for %q — a transaction on another connection would not be atomic with it",
			ErrNestedMismatch, root.connection, req.GetConnectionName())
	}
	if iso := req.GetIsolation(); iso != distxpb.Isolation_ISOLATION_UNSPECIFIED && iso != root.isolation {
		return nil, ctx, fmt.Errorf("%w: it runs at isolation %s and this Begin asks for %s — a running transaction cannot change level",
			ErrNestedMismatch, root.isolation, iso)
	}
	h := &TxHandle{
		client:     root.client,
		txID:       root.txID,
		connID:     root.connID,
		connection: root.connection,
		isolation:  root.isolation,
		scope:      root.scope,
		joined:     true,
	}
	return h, h.attach(ctx), nil
}

// Commit finalises the transaction. The conn_id (when the proxy
// supplied one) is re-attached to ctx as outgoing metadata so the
// proxy routes the Commit to the pinned replica before evicting its
// affinity entry; the tx_id travels in the request body as before.
//
// ctx may be a fresh context (the handle re-attaches its own routing
// metadata); whatever else the caller put on it is preserved.
//
// The work parked on this transaction (an emit waiting for it to be durable)
// runs after a successful commit and is dropped otherwise. A JOINED handle
// commits nothing: the root does. A root whose nested part rolled back is
// rolled back here instead, and Commit returns [ErrRollbackOnly].
func (h *TxHandle) Commit(ctx context.Context) error {
	if h.joined {
		return nil
	}
	if h.scope != nil && h.scope.RollbackOnly() {
		h.scope.Discarded()
		_, rerr := h.client.Rollback(h.attach(context.WithoutCancel(ctx)), &distxpb.RollbackRequest{TxId: h.txID})
		if rerr != nil {
			return errors.Join(ErrRollbackOnly, fmt.Errorf("the rollback also failed: %w", rerr))
		}
		return ErrRollbackOnly
	}
	_, err := h.client.Commit(h.attach(ctx), &distxpb.CommitRequest{TxId: h.txID})
	if h.scope == nil {
		return err
	}
	if err != nil {
		h.scope.Discarded()
		return err
	}
	// The transaction IS committed from here on, whatever the parked work
	// does: a panicking emit must not unwind into a caller that would then
	// roll back a transaction that already landed (distx.Run's deferred
	// rollback). txscope runs every hook and hands back what panicked.
	for _, p := range h.scope.Committed() {
		observx.ReportError(context.WithoutCancel(ctx),
			fmt.Errorf("distx: work parked on committed transaction %s panicked: %v", h.txID, p))
	}
	return nil
}

// Rollback discards the transaction. Like [TxHandle.Commit], it
// re-attaches the conn_id so the proxy routes the Rollback to the
// pinned replica before evicting.
//
// A JOINED handle cannot roll back its part alone, so it marks the root
// rollback-only and the root's Commit rolls the whole transaction back.
//
// The rollback outlives ctx's cancellation (its values and metadata are kept).
// The commonest reason to roll back is that the request's context is done —
// the client hung up, the deadline passed — and a gRPC client refuses a call on
// a done context before it leaves the process. So exactly when the rollback
// mattered most it did not happen, and the transaction stayed held until the
// server timed it out. distx.Run already detached its own rollback; every
// hand-written `defer tx.Rollback(ctx)` did not, and now does not have to.
func (h *TxHandle) Rollback(ctx context.Context) error {
	if h.joined {
		if h.scope != nil {
			h.scope.MarkRollbackOnly()
		}
		return nil
	}
	if h.scope != nil {
		h.scope.Discarded()
	}
	_, err := h.client.Rollback(h.attach(context.WithoutCancel(ctx)), &distxpb.RollbackRequest{TxId: h.txID})
	return err
}

// TxID returns the transaction id the caller threads as the w17-tx-id
// header (already attached on the ctx [Begin] returned).
func (h *TxHandle) TxID() string { return h.txID }

// ConnID returns the proxy routing token, or "" in single-replica
// deployments where no proxy minted one.
func (h *TxHandle) ConnID() string { return h.connID }

// attach derives a ctx carrying this tx's outgoing routing metadata.
// w17-tx-id is always set; w17-conn-id is set when the proxy supplied
// one and REMOVED otherwise — so single-replica output carries only
// w17-tx-id. Everything else the caller put on ctx is kept.
//
// SET, not appended. Appending was additive, so a ctx that already
// carried a transaction's headers — a nested Begin, or a context kept
// past the end of its transaction — went out with two `w17-tx-id`
// values, and storage adopts the first: the wrong transaction.
func (h *TxHandle) attach(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(txregistry.HeaderName, h.txID)
	if h.connID != "" {
		md.Set(ConnIDHeader, h.connID)
	} else {
		md.Delete(ConnIDHeader)
	}
	return metadata.NewOutgoingContext(ctx, md)
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
