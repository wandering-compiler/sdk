//! Distributed transactions — the Rust twin of sdk/go/service/tx/distx.
//!
//! The transaction lives in the storage server (`W17DistributedTransaction`
//! Begin / Commit / Rollback). Every storage call that carries `w17-tx-id`
//! adopts it; `w17-conn-id`, when the storage proxy minted one in Begin's
//! response headers, pins the call to the replica that holds it. This module
//! is the client half, with Go's semantics:
//!
//! - [`run`] — begin, run the closure with the transaction's context, commit
//!   when it returns `Ok`, roll back when it returns `Err` (the error keeps
//!   propagating) or when its future is dropped (the caller hung up, or its
//!   deadline passed: dropping is Rust's cancellation);
//! - [`begin`] → [`TxHandle::commit`] / [`TxHandle::rollback`] for the
//!   manual form;
//! - NESTED: a begin on a context that already belongs to an open
//!   transaction JOINS it — no RPC; its commit is a no-op and its rollback
//!   marks the root rollback-only. A join needs the same connection and an
//!   unspecified or equal isolation, or it is [`Error::NestedMismatch`];
//! - the root's commit after a joined rollback rolls back and is
//!   [`Error::RollbackOnly`];
//! - a second commit or rollback on a finished root handle is
//!   [`Error::HandleFinished`], without an RPC;
//! - a commit that fails in transport is probed with a detached rollback: if
//!   it succeeds the commit did not take effect (an ordinary failure); if not,
//!   [`Error::CommitOutcomeUnknown`] — the writes may have landed, and the
//!   work parked for the commit is NOT run;
//! - rollback outlives the caller's cancellation and deadline; a done context
//!   refuses to commit ([`run`]);
//! - `ABORTED` from storage is returned to the caller, who retries the unit
//!   of work.
//!
//! Work parked with [`crate::on_commit`] (and every event emitted inside the
//! transaction) runs after the root commit, in order, and is dropped on
//! rollback.

use std::{fmt, future::Future, sync::Arc, time::Duration};

use tonic::{Code, Status};

use crate::{
    channel::StorageChannel,
    ctx::Ctx,
    observability,
    pb::w17::common::distx::{
        BeginRequest, CommitRequest, Isolation, RollbackRequest,
        w17_distributed_transaction_client::W17DistributedTransactionClient,
    },
    txscope::{self, Scope},
};

/// txregistry.HeaderName / distx.ConnIDHeader.
pub const TX_ID_HEADER: &str = "w17-tx-id";
pub const CONN_ID_HEADER: &str = "w17-conn-id";

/// Bounds the rollback a commit sends to settle an ambiguous outcome.
pub const OUTCOME_PROBE_TIMEOUT: Duration = Duration::from_secs(10);

/// The `BeginRequest` for [`begin`] / [`run`]. `connection` is the
/// connection's name in the lock (`shop-postgres`): the storage tier's
/// coordinator opens the transaction on it and refuses a name it does not
/// serve, the empty one included (`INVALID_ARGUMENT`, "unknown connection").
pub fn begin_request(connection: &str) -> BeginRequest {
    BeginRequest {
        connection_name: connection.to_string(),
        tx_timeout_ms: 0,
        isolation: Isolation::Unspecified as i32,
    }
}

/// A transaction error. As a status it is `UNKNOWN` with its message — a Go
/// *returned* error — except [`Error::Rpc`], the coordinator's own status,
/// which is returned unchanged.
#[derive(Debug, Clone)]
pub enum Error {
    /// A JOINED part rolled back, so the root rolled the whole transaction
    /// back instead of committing it.
    RollbackOnly(String),
    /// A begin found a transaction open on the context but cannot JOIN it:
    /// another connection, or another isolation asked for explicitly.
    NestedMismatch(String),
    /// A second commit or rollback on a root handle whose transaction already
    /// ended through it.
    HandleFinished(String),
    /// The commit failed in transport and the transaction could no longer be
    /// rolled back: the commit may have landed. Do not assume either outcome.
    CommitOutcomeUnknown(String),
    /// [`run`] did not commit: the caller's context is done.
    NotCommitted(String),
    /// A transaction cannot be opened here (a bidirectional-streaming method).
    Refused(String),
    /// The coordinator answered with this status.
    Rpc(Status),
}

impl Error {
    /// The error's name — `"RollbackOnly"`, `"NestedMismatch"`,
    /// `"HandleFinished"`, `"CommitOutcomeUnknown"`, `"NotCommitted"`,
    /// `"Refused"`, `"Rpc"`.
    pub fn name(&self) -> &'static str {
        match self {
            Error::RollbackOnly(_) => "RollbackOnly",
            Error::NestedMismatch(_) => "NestedMismatch",
            Error::HandleFinished(_) => "HandleFinished",
            Error::CommitOutcomeUnknown(_) => "CommitOutcomeUnknown",
            Error::NotCommitted(_) => "NotCommitted",
            Error::Refused(_) => "Refused",
            Error::Rpc(_) => "Rpc",
        }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::RollbackOnly(m)
            | Error::NestedMismatch(m)
            | Error::HandleFinished(m)
            | Error::CommitOutcomeUnknown(m)
            | Error::NotCommitted(m)
            | Error::Refused(m) => f.write_str(m),
            Error::Rpc(s) => write!(f, "{:?}: {}", s.code(), s.message()),
        }
    }
}

impl std::error::Error for Error {}

impl From<Error> for Status {
    fn from(e: Error) -> Self {
        match e {
            Error::Rpc(s) => s,
            Error::Refused(m) => Status::failed_precondition(m),
            other => Status::unknown(other.to_string()),
        }
    }
}

fn rollback_only(detail: &str) -> Error {
    let mut m =
        "distx: a nested part of this transaction rolled back, so the transaction was rolled back instead of committed"
            .to_string();
    if !detail.is_empty() {
        m.push_str("; ");
        m.push_str(detail);
    }
    Error::RollbackOnly(m)
}

/// The coordinator client [`begin`] drives:
/// `clients.distributed_transaction()?` returns one.
#[derive(Clone)]
pub struct Coordinator {
    client: W17DistributedTransactionClient<StorageChannel>,
}

impl Coordinator {
    pub(crate) fn new(channel: StorageChannel) -> Self {
        Self {
            client: W17DistributedTransactionClient::new(channel),
        }
    }

    async fn begin(&self, ctx: &Ctx, req: BeginRequest) -> Result<(String, String), Status> {
        let resp = self.client.clone().begin(ctx.request(req)).await?;
        let conn_id = resp
            .metadata()
            .get_all(CONN_ID_HEADER)
            .iter()
            .filter_map(|v| v.to_str().ok())
            .find(|v| !v.is_empty())
            .unwrap_or("")
            .to_string();
        Ok((resp.into_inner().tx_id, conn_id))
    }

    async fn commit(&self, ctx: &Ctx, tx_id: &str) -> Result<(), Status> {
        self.client
            .clone()
            .commit(ctx.request(CommitRequest {
                tx_id: tx_id.to_string(),
            }))
            .await
            .map(|_| ())
    }

    async fn rollback(&self, ctx: &Ctx, tx_id: &str) -> Result<(), Status> {
        self.client
            .clone()
            .rollback(ctx.request(RollbackRequest {
                tx_id: tx_id.to_string(),
            }))
            .await
            .map(|_| ())
    }
}

/// The root transaction a context belongs to.
pub(crate) struct Root {
    coord: Coordinator,
    tx_id: String,
    conn_id: String,
    connection: String,
    isolation: i32,
    pub(crate) scope: Arc<Scope>,
    /// How the root handle ended ("committed", "rolled back", …), `None`
    /// while it is open; also the lock one commit or rollback holds.
    ended: tokio::sync::Mutex<Option<&'static str>>,
}

impl Root {
    fn attach(&self, ctx: &Ctx) -> Ctx {
        // SET, never appended: an appended second w17-tx-id is adopted by
        // nobody; storage adopts the first.
        let mut out = ctx.outgoing().with_set(TX_ID_HEADER, [self.tx_id.as_str()]);
        out = if self.conn_id.is_empty() {
            out.with_set(CONN_ID_HEADER, Vec::<String>::new())
        } else {
            out.with_set(CONN_ID_HEADER, [self.conn_id.as_str()])
        };
        ctx.with_outgoing(out)
    }

    fn finished(&self, how: &str) -> Error {
        Error::HandleFinished(format!(
            "distx: this transaction handle was already used: transaction {} was already {how}",
            self.tx_id
        ))
    }
}

/// One distributed transaction opened by [`begin`]. Single use: one commit
/// OR one rollback; a failed attempt does not use it up.
#[derive(Clone)]
pub struct TxHandle {
    root: Arc<Root>,
    joined: bool,
}

impl TxHandle {
    /// The transaction id.
    pub fn tx_id(&self) -> &str {
        &self.root.tx_id
    }

    /// The proxy routing token, "" when no proxy minted one.
    pub fn conn_id(&self) -> &str {
        &self.root.conn_id
    }

    /// Whether this handle joined a transaction opened further out.
    pub fn joined(&self) -> bool {
        self.joined
    }

    /// `ctx` with this transaction's routing headers set.
    pub fn attach(&self, ctx: &Ctx) -> Ctx {
        self.root.attach(ctx)
    }

    /// Commit; a joined handle commits nothing (the root does).
    ///
    /// Runs the parked work after a successful commit (failures reported,
    /// never returned); drops it otherwise. The commit and that work run to
    /// their end even when the caller's future is dropped.
    pub async fn commit(&self, ctx: &Ctx) -> Result<(), Error> {
        if self.joined {
            return Ok(());
        }
        let root = self.root.clone();
        let ctx = ctx.clone();
        txscope::shielded(async move { commit_root(root, ctx).await })
            .await
            .unwrap_or_else(|e| Err(Error::Rpc(Status::internal(e))))
    }

    /// Roll back. A joined handle marks the root rollback-only instead.
    ///
    /// Outlives the context's cancellation and deadline: the commonest reason
    /// to roll back is that the caller is gone, and a call refused on a done
    /// context would leave the transaction held until the server times it
    /// out.
    pub async fn rollback(&self, ctx: &Ctx) -> Result<(), Error> {
        if self.joined {
            self.root.scope.mark_rollback_only();
            return Ok(());
        }
        let root = self.root.clone();
        let ctx = ctx.without_cancel();
        txscope::shielded(async move { rollback_root(root, ctx).await })
            .await
            .unwrap_or_else(|e| Err(Error::Rpc(Status::internal(e))))
    }
}

async fn commit_root(root: Arc<Root>, ctx: Ctx) -> Result<(), Error> {
    let result = {
        let mut ended = root.ended.lock().await;
        if let Some(how) = *ended {
            return Err(root.finished(how));
        }
        if root.scope.rollback_only() {
            root.scope.discarded();
            if let Err(rerr) = root
                .coord
                .rollback(&root.attach(&ctx.without_cancel()), &root.tx_id)
                .await
            {
                return Err(rollback_only(&format!(
                    "the rollback also failed: {}",
                    rerr.message()
                )));
            }
            *ended = Some("rolled back");
            return Err(rollback_only(""));
        }
        match root.coord.commit(&root.attach(&ctx), &root.tx_id).await {
            Ok(()) => {
                *ended = Some("committed");
                Ok(())
            }
            Err(e) if outcome_unknown(&e) => {
                // Used up BEFORE the settle: nothing may commit this handle
                // again while the outcome is being settled.
                *ended = Some("left with its outcome unknown");
                let settled = settle(&root, &ctx, e).await;
                if settled.is_ok() {
                    *ended = Some("rolled back");
                }
                Err(settled.unwrap_or_else(|e| e))
            }
            Err(e) => Err(Error::Rpc(e)),
        }
    };
    if let Err(e) = result {
        root.scope.discarded();
        return Err(e);
    }
    // The transaction IS committed from here on: the parked work (the events
    // of the writes among it) runs to its end, and every failure is
    // reported, never returned into a commit that landed.
    for failure in root.scope.run_committed().await {
        observability::report_error(&format!(
            "distx: work parked on committed transaction {} failed: {failure}",
            root.tx_id
        ));
    }
    Ok(())
}

/// After a commit with an unknown outcome: roll back, detached. `Ok` (with
/// the commit's own error) means the transaction was still open, so the
/// commit never took effect; `Err` means it is gone and the outcome is
/// unknown.
async fn settle(root: &Root, ctx: &Ctx, commit_err: Status) -> Result<Error, Error> {
    let probe = ctx.without_cancel().with_timeout(OUTCOME_PROBE_TIMEOUT);
    match root.coord.rollback(&root.attach(&probe), &root.tx_id).await {
        Ok(()) => Ok(Error::Rpc(commit_err)),
        Err(rerr) => {
            observability::report_error(&format!(
                "distx: the outcome of committing transaction {} is unknown — the commit call failed ({}) and the \
                 transaction could no longer be rolled back ({}); its writes may have landed, and {} piece(s) of work \
                 waiting for the commit were not run",
                root.tx_id,
                commit_err.message(),
                rerr.message(),
                root.scope.pending(),
            ));
            root.scope.discarded();
            Err(Error::CommitOutcomeUnknown(format!(
                "distx: the commit's outcome is unknown: transaction {}: {}",
                root.tx_id,
                commit_err.message()
            )))
        }
    }
}

async fn rollback_root(root: Arc<Root>, ctx: Ctx) -> Result<(), Error> {
    let mut ended = root.ended.lock().await;
    if let Some(how) = *ended {
        return Err(root.finished(how));
    }
    root.scope.discarded();
    root.coord
        .rollback(&root.attach(&ctx), &root.tx_id)
        .await
        .map_err(Error::Rpc)?;
    *ended = Some("rolled back");
    Ok(())
}

/// A commit failure that does not say whether the server committed: it
/// failed in transport or was abandoned rather than answered.
fn outcome_unknown(st: &Status) -> bool {
    matches!(
        st.code(),
        Code::Unavailable | Code::DeadlineExceeded | Code::Cancelled
    )
}

/// Open (or JOIN) a distributed transaction.
///
/// Returns the handle and a context carrying `w17-tx-id` (and `w17-conn-id`
/// when the proxy supplied one): pass that context to every storage call
/// that belongs to the transaction.
pub async fn begin(
    ctx: &Ctx,
    coord: &Coordinator,
    req: BeginRequest,
) -> Result<(TxHandle, Ctx), Error> {
    if ctx.is_bidi() {
        return Err(Error::Refused(
            "distx: a bidirectional-streaming method cannot run a transaction (as in Go): its messages \
             interleave with the caller's, and nothing would bound the transaction"
                .into(),
        ));
    }
    if let Some(root) = ctx.tx()
        && root.scope.active()
    {
        return join(ctx, root, &req);
    }
    let (tx_id, conn_id) = coord.begin(ctx, req.clone()).await.map_err(Error::Rpc)?;
    let root = Arc::new(Root {
        coord: coord.clone(),
        tx_id,
        conn_id,
        connection: req.connection_name,
        isolation: req.isolation,
        scope: Arc::new(Scope::default()),
        ended: tokio::sync::Mutex::new(None),
    });
    let tx_ctx = root.attach(&ctx.with_tx(Some(root.clone())));
    Ok((
        TxHandle {
            root,
            joined: false,
        },
        tx_ctx,
    ))
}

fn join(ctx: &Ctx, root: &Arc<Root>, req: &BeginRequest) -> Result<(TxHandle, Ctx), Error> {
    if req.connection_name != root.connection {
        return Err(Error::NestedMismatch(format!(
            "distx: cannot join the transaction already open on this context: it is on connection {:?} and this \
             begin asks for {:?} — a transaction on another connection would not be atomic with it",
            root.connection, req.connection_name
        )));
    }
    if req.isolation != Isolation::Unspecified as i32 && req.isolation != root.isolation {
        return Err(Error::NestedMismatch(format!(
            "distx: cannot join the transaction already open on this context: it runs at isolation {} and this \
             begin asks for {} — a running transaction cannot change level",
            root.isolation, req.isolation
        )));
    }
    // SET the root's routing headers even when ctx already shows them.
    Ok((
        TxHandle {
            root: root.clone(),
            joined: true,
        },
        root.attach(ctx),
    ))
}

/// Rolls a transaction back when the future running it is dropped before it
/// ended (the caller hung up, a deadline passed, a panic unwound): a joined
/// part marks the root rollback-only, a root rolls back in a tracked task.
struct DropGuard {
    handle: Option<TxHandle>,
    detached: Ctx,
}

impl Drop for DropGuard {
    fn drop(&mut self) {
        let Some(h) = self.handle.take() else { return };
        if h.joined {
            h.root.scope.mark_rollback_only();
            return;
        }
        let ctx = self.detached.clone();
        let Ok(rt) = tokio::runtime::Handle::try_current() else {
            h.root.scope.discarded();
            return;
        };
        let _enter = rt.enter();
        txscope::spawn_tracked(async move {
            if let Err(e) = rollback_root(h.root.clone(), ctx).await {
                observability::report_error(&format!(
                    "distx: rolling back transaction {} after its caller went away: {e}",
                    h.root.tx_id
                ));
            }
        });
    }
}

/// Run `f` inside one distributed transaction and return its result.
///
/// Commit when `f` returns `Ok`; roll back (detached) when it returns `Err`
/// — the error keeps propagating — or when this future is dropped before it
/// finished. A caller whose context is done gets no commit
/// ([`Error::NotCommitted`]). A failed commit is returned as it is, and
/// `f`'s work is lost.
///
/// `f` must use the context it receives for every write of the transaction.
/// `E` is any error a [`Error`] converts into — `tonic::Status`, or
/// [`Error`] itself.
///
/// ```ignore
/// distx::run(&ctx, &self.tx, distx::begin_request("shop-postgres"), async |tx_ctx| {
///     let order = self.orders.clone().create_order(tx_ctx.request(req)).await?.into_inner();
///     Ok(order)
/// }).await
/// ```
pub async fn run<T, E, F, Fut>(
    ctx: &Ctx,
    coord: &Coordinator,
    req: BeginRequest,
    f: F,
) -> Result<T, E>
where
    F: FnOnce(Ctx) -> Fut,
    Fut: Future<Output = Result<T, E>>,
    E: From<Error>,
{
    let (handle, tx_ctx) = begin(ctx, coord, req).await?;
    let detached = ctx.without_cancel();
    let mut guard = DropGuard {
        handle: Some(handle),
        detached: detached.clone(),
    };
    let result = f(tx_ctx).await;
    let handle = guard
        .handle
        .take()
        .expect("the guard holds the handle until f returns");
    match result {
        Err(e) => {
            if let Err(rerr) = handle.rollback(&detached).await {
                observability::report_error(&format!("distx.run: rollback also failed: {rerr}"));
            }
            Err(e)
        }
        Ok(v) => {
            if let Some(why) = ctx.err() {
                let _ = handle.rollback(&detached).await;
                return Err(Error::NotCommitted(format!(
                    "distx.run: not committing, the caller's context is done: {why}"
                ))
                .into());
            }
            handle.commit(ctx).await?;
            Ok(v)
        }
    }
}
