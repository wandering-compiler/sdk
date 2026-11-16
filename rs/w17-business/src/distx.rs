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
//!   it succeeds the commit did not take effect (an ordinary failure). When
//!   the transaction is gone but the coordinator remembers how it ended (the
//!   `ended` detail on its `NOT_FOUND`), that settles it: success and the
//!   parked work runs when it committed, an ordinary failure when it did not.
//!   Only a coordinator that cannot say leaves
//!   [`Error::CommitOutcomeUnknown`] — the writes may have landed, and the
//!   work parked for the commit is NOT run;
//! - a commit refused with `FAILED_PRECONDITION` (a call inside the
//!   transaction still holds it) leaves the transaction open: the handle is
//!   retryable and the parked work waits for the retry. [`run`] rolls such a
//!   transaction back rather than leave it holding its locks until the
//!   orphan timeout;
//! - rollback outlives the caller's cancellation and deadline and is bounded
//!   by [`ROLLBACK_WAIT`] of its own; a rollback of a transaction that
//!   already ended without committing is done, not a failure. A done context
//!   refuses to commit ([`run`]);
//! - `ABORTED` from storage is returned to the caller, who retries the unit
//!   of work.
//!
//! Work parked with [`crate::on_commit`] (and every event emitted inside the
//! transaction) runs after the root commit, in order, and is dropped on
//! rollback.
//!
//! A method reached over the wire inside a transaction ANOTHER process runs
//! (its request carries that process's `w17-tx-id`, and nothing in this
//! process opened it) parks its events with [`RemoteCommits`]: the
//! coordinator's `W17TransactionOutcome.Await` says how that transaction
//! ends.

use std::{fmt, future::Future, sync::Arc, time::Duration};

use tonic::{Code, Status};

use crate::{
    channel::StorageChannel,
    ctx::Ctx,
    metadata::Metadata,
    observability,
    pb::w17::common::distx::{
        AwaitRequest, BeginRequest, CommitRequest, Isolation, RollbackRequest, TxOutcome,
        w17_distributed_transaction_client::W17DistributedTransactionClient,
        w17_transaction_outcome_client::W17TransactionOutcomeClient,
    },
    txscope::{self, Hook, Scope},
};

/// txregistry.HeaderName / distx.ConnIDHeader.
pub const TX_ID_HEADER: &str = "w17-tx-id";
pub const CONN_ID_HEADER: &str = "w17-conn-id";

/// Bounds the rollback a commit sends to settle an ambiguous outcome.
pub const OUTCOME_PROBE_TIMEOUT: Duration = Duration::from_secs(10);

/// Bounds every rollback sent on a context detached from its caller's
/// ([`TxHandle::rollback`], the rollback-only commit, [`run`]'s rollback, the
/// rollback after a dropped future). Detaching drops the caller's deadline
/// too, so without a bound a coordinator that accepted the call and stalls
/// would hold the caller forever. The server dooms the transaction before it
/// waits for a running handler's lease, so a rollback cut by this bound is
/// still decided. Go's rollbackWait: the finish wait, equal to the
/// auto-rollback interceptor's RollbackWait.
pub const ROLLBACK_WAIT: Duration = Duration::from_secs(30);

/// The `google.rpc.ErrorInfo` the coordinator attaches to a `NOT_FOUND` for a
/// transaction it remembers (its last 256 ids): `metadata[ENDED_KEY]` is
/// "committed" or "not_committed". Go's distx.EndedDomain / EndedReason /
/// EndedKey.
pub const ENDED_DOMAIN: &str = "w17.distx";
pub const ENDED_REASON: &str = "W17_TX_ENDED";
pub const ENDED_KEY: &str = "ended";

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
    /// [`ROLLBACK_WAIT`]; tests shorten it.
    rollback_wait: Duration,
}

impl Coordinator {
    pub(crate) fn new(channel: StorageChannel) -> Self {
        Self {
            client: W17DistributedTransactionClient::new(channel),
            rollback_wait: ROLLBACK_WAIT,
        }
    }

    #[cfg(test)]
    pub(crate) fn with_rollback_wait(mut self, d: Duration) -> Self {
        self.rollback_wait = d;
        self
    }

    /// `ctx` without its cancellation and deadline (values and metadata
    /// kept), bounded by the rollback wait.
    fn detached_rollback_ctx(&self, ctx: &Ctx) -> Ctx {
        ctx.without_cancel().with_timeout(self.rollback_wait)
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
    /// out. It is bounded by [`ROLLBACK_WAIT`] instead, so a stalled
    /// coordinator cannot hold the caller forever. A transaction that already
    /// ended without committing is rolled back as asked: `Ok`.
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
                .rollback(
                    &root.attach(&root.coord.detached_rollback_ctx(&ctx)),
                    &root.tx_id,
                )
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
                match settle(&root, &ctx, e).await {
                    Settled::Committed => {
                        *ended = Some("committed");
                        Ok(())
                    }
                    Settled::RolledBack(e) => {
                        *ended = Some("rolled back");
                        Err(e)
                    }
                    Settled::Unknown(e) => Err(e),
                }
            }
            Err(e) => Err(Error::Rpc(e)),
        }
    };
    if let Err(e) = result {
        // Only an answer that ENDED the transaction drops its parked work. A
        // refusal that left it open (a call still holds it) is retryable on
        // this handle, and the retry's commit must still run the work waiting
        // for it.
        if !left_open(&e) {
            root.scope.discarded();
        }
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

/// How a commit with an unknown outcome settled.
enum Settled {
    /// The coordinator recorded the commit as landed; only its reply was lost.
    Committed,
    /// The transaction did not commit (the probe rolled it back, or the
    /// coordinator recorded it as ended without committing): the commit's
    /// own error.
    RolledBack(Error),
    /// Nobody can say.
    Unknown(Error),
}

/// After a commit with an unknown outcome: roll back, detached. Success
/// means the transaction was still open, so the commit never took effect. A
/// `NOT_FOUND` carrying the coordinator's ended detail settles it from its
/// record. Any other failure means it is gone and the outcome is unknown.
async fn settle(root: &Root, ctx: &Ctx, commit_err: Status) -> Settled {
    let probe = ctx.without_cancel().with_timeout(OUTCOME_PROBE_TIMEOUT);
    match root.coord.rollback(&root.attach(&probe), &root.tx_id).await {
        Ok(()) => Settled::RolledBack(Error::Rpc(commit_err)),
        Err(rerr) if ended_from(&rerr) == Ended::Committed => Settled::Committed,
        Err(rerr) if ended_from(&rerr) == Ended::NotCommitted => {
            Settled::RolledBack(Error::Rpc(commit_err))
        }
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
            Settled::Unknown(Error::CommitOutcomeUnknown(format!(
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
    let rctx = root.coord.detached_rollback_ctx(&ctx);
    if let Err(st) = root.coord.rollback(&root.attach(&rctx), &root.tx_id).await {
        match ended_from(&st) {
            // Already ended without committing — the auto-rollback
            // interceptor discarded it after a failed call, or its orphan
            // bound ran out. That is what this rollback asked for, so it is
            // not a failure.
            Ended::NotCommitted => {}
            Ended::Committed => {
                *ended = Some("committed");
                return Err(Error::Rpc(Status::with_details(
                    st.code(),
                    format!(
                        "distx: cannot roll back transaction {}, it was already committed: {}",
                        root.tx_id,
                        st.message()
                    ),
                    st.details().to_vec().into(),
                )));
            }
            Ended::Unknown => return Err(Error::Rpc(st)),
        }
    }
    *ended = Some("rolled back");
    Ok(())
}

/// A commit (or rollback) failure after which the transaction is still open:
/// the coordinator refused to finish it because a call inside it still holds
/// it past the finish wait (`FAILED_PRECONDITION`). The handle is not used
/// up; the caller retries, or rolls back ([`run`] rolls back).
fn left_open(e: &Error) -> bool {
    matches!(e, Error::Rpc(st) if st.code() == Code::FailedPrecondition)
}

/// The coordinator's record of how a transaction it no longer holds ended.
#[derive(Debug, PartialEq, Eq)]
enum Ended {
    Committed,
    NotCommitted,
    Unknown,
}

/// Read from the `ErrorInfo` on the coordinator's `NOT_FOUND`;
/// [`Ended::Unknown`] for any other status or a `NOT_FOUND` without it.
fn ended_from(st: &Status) -> Ended {
    if st.code() != Code::NotFound {
        return Ended::Unknown;
    }
    for info in crate::errors::error_infos(st) {
        if info.domain != ENDED_DOMAIN || info.reason != ENDED_REASON {
            continue;
        }
        match info.metadata.get(ENDED_KEY).map(String::as_str) {
            Some("committed") => return Ended::Committed,
            Some("not_committed") => return Ended::NotCommitted,
            _ => {}
        }
    }
    Ended::Unknown
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
/// `f`'s work is lost; a commit refused because a call still held the
/// transaction is rolled back first.
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
                // The refusal is what the caller acts on; a failed rollback
                // is named in it, as Go's Run does — the transaction may be
                // left open until the server times it out.
                let also = match handle.rollback(&detached).await {
                    Ok(()) => String::new(),
                    Err(rerr) => format!(" (rollback also failed: {rerr})"),
                };
                return Err(Error::NotCommitted(format!(
                    "distx.run: not committing, the caller's context is done: {why}{also}"
                ))
                .into());
            }
            if let Err(e) = handle.commit(ctx).await {
                // A refusal that left the transaction open (a call inside it
                // still held it past the finish wait) is the one commit
                // failure after which a rollback is still owed: run reports
                // the failure, so it must not leave the transaction holding
                // its locks until the orphan timeout. The server dooms it at
                // once and rolls it back when the holder releases. Every
                // other answer already ended it.
                if left_open(&e)
                    && let Err(rerr) = handle.rollback(&detached).await
                {
                    observability::report_error(&format!(
                        "distx.run: rollback also failed: {rerr}"
                    ));
                }
                return Err(e.into());
            }
            Ok(v)
        }
    }
}

// --- a transaction another process runs ------------------------------------------

/// Bounds how long [`RemoteCommits`] waits for the coordinator to confirm it
/// is watching a transaction. The method is answering its caller meanwhile,
/// so the bound is short; a coordinator that cannot confirm in time is
/// treated as one that cannot say how the transaction ends. Go's
/// registerWait.
pub const REGISTER_WAIT: Duration = Duration::from_secs(10);

/// What [`RemoteCommits::defer_remote`] did with the work it was handed.
pub(crate) enum Remote {
    /// The transaction had already committed, or its coordinator predates
    /// the outcome service: the caller runs the work now.
    Now(Hook),
    /// The transaction is open: the work runs when it commits and is dropped
    /// when it does not.
    Deferred,
    /// It had already ended without committing, the coordinator does not
    /// know it, or cannot be asked: the work is dropped, and that is
    /// reported.
    Dropped,
}

/// The commit hook of a business tier reached over the wire inside a
/// transaction ANOTHER process runs (Go's `distx.RemoteCommits`): the request
/// carries that process's `w17-tx-id`, so no in-process scope will ever hear
/// how it ends.
///
/// It asks the transaction's coordinator (`W17TransactionOutcome.Await`,
/// served beside `W17DistributedTransaction` by the storage bundle that
/// minted the id, on the same connection) and waits for the answer: the work
/// runs once when the transaction commits and never when it does not — what
/// the same method's events do inside one process. The wait is registered
/// before the method answers its caller, so the caller's commit cannot
/// outrun it.
///
/// Delivery is at-most-once, as everywhere after a commit: a process that
/// stops while it waits, or a coordinator that cannot say how the
/// transaction ended, loses the work, and that is reported.
#[derive(Clone)]
pub(crate) struct RemoteCommits {
    client: W17TransactionOutcomeClient<StorageChannel>,
    /// [`REGISTER_WAIT`]; tests shorten it.
    register_wait: Duration,
}

impl RemoteCommits {
    pub(crate) fn new(channel: StorageChannel) -> Self {
        Self {
            client: W17TransactionOutcomeClient::new(channel),
            register_wait: REGISTER_WAIT,
        }
    }

    #[cfg(test)]
    pub(crate) fn with_register_wait(mut self, d: Duration) -> Self {
        self.register_wait = d;
        self
    }

    /// The transaction a request reached this process inside, when another
    /// process runs it: the request's `w17-tx-id`. `None` when there is none,
    /// or when `ctx` belongs to a transaction this process opened (the
    /// in-process scope settles that one).
    pub(crate) fn remote_tx(ctx: &Ctx) -> Option<String> {
        if ctx.tx().is_some() {
            return None;
        }
        first_non_empty(ctx.incoming(), TX_ID_HEADER)
    }

    /// Register a wait for `tx_id` at the coordinator and park `hook` on it.
    ///
    /// `ctx` is the request's: its `w17-conn-id` (the storage proxy's routing
    /// token) is echoed so the wait reaches the replica holding the
    /// transaction, and nothing else of it goes along — not its principal,
    /// its other metadata, nor its cancellation or deadline: the request ends
    /// long before the commit.
    pub(crate) async fn defer_remote(&self, ctx: &Ctx, tx_id: &str, hook: Hook) -> Remote {
        let mut md = Metadata::new();
        if let Some(conn) = first_non_empty(ctx.incoming(), CONN_ID_HEADER) {
            md = md.with_set(CONN_ID_HEADER, [conn]);
        }
        let wctx = Ctx::background().with_outgoing(md);
        let mut client = self.client.clone();
        let req = wctx.request(AwaitRequest {
            tx_id: tx_id.to_string(),
        });
        let first = tokio::time::timeout(self.register_wait, async move {
            let mut stream = client.r#await(req).await?.into_inner();
            let msg = stream.message().await?;
            Ok::<_, Status>((stream, msg))
        })
        .await;
        let (mut stream, first) = match first {
            Err(_) => {
                return unwatched(
                    tx_id,
                    format!(
                        "the coordinator did not confirm the wait within {}",
                        humantime::format_duration(self.register_wait)
                    ),
                );
            }
            Ok(Err(st)) if st.code() == Code::Unimplemented => {
                // A coordinator older than the outcome service: it cannot be
                // asked, and dropping every event in such a mixed deployment
                // would be a regression nobody asked for. Announced now, as
                // before — said.
                observability::report_error(&format!(
                    "distx: announcing inside transaction {tx_id:?} before it commits: its coordinator does not \
                     serve W17TransactionOutcome (upgrade the storage bundle), so the event cannot wait for the \
                     commit"
                ));
                return Remote::Now(hook);
            }
            Ok(Err(st)) => return unwatched(tx_id, status_text(&st)),
            Ok(Ok((_, None))) => {
                return unwatched(tx_id, "it ended the wait without an answer".into());
            }
            Ok(Ok((stream, Some(msg)))) => (stream, msg.outcome()),
        };
        match first {
            TxOutcome::Committed => return Remote::Now(hook),
            TxOutcome::NotCommitted => {
                observability::report_error(&format!(
                    "distx: dropped the deferred announcement of tx {tx_id:?}: the transaction ended without \
                     committing before the emit registered"
                ));
                return Remote::Dropped;
            }
            TxOutcome::Pending => {}
        }
        // Tracked like every other piece of work a commit owes: the shutdown
        // drain waits for it and counts it when it cannot.
        let tx_id = tx_id.to_string();
        txscope::spawn_tracked(async move {
            loop {
                let msg = match stream.message().await {
                    Ok(Some(msg)) => msg,
                    Ok(None) => {
                        return stopped(&tx_id, "the stream ended");
                    }
                    Err(st) => return stopped(&tx_id, &status_text(&st)),
                };
                match msg.outcome() {
                    TxOutcome::Committed => {
                        if let Err(e) = txscope::run_hook(hook).await {
                            observability::report_error(&format!(
                                "distx: work parked on committed transaction {tx_id} failed: {e}"
                            ));
                        }
                        return;
                    }
                    // Rolled back: the writes are gone, and so is their
                    // announcement.
                    TxOutcome::NotCommitted => return,
                    TxOutcome::Pending => {}
                }
            }
        });
        Remote::Deferred
    }
}

fn first_non_empty(md: &Metadata, key: &str) -> Option<String> {
    md.get_all(key).iter().find(|v| !v.is_empty()).cloned()
}

fn status_text(st: &Status) -> String {
    format!("{:?}: {}", st.code(), st.message())
}

/// Work whose wait could not be registered is dropped, said.
fn unwatched(tx_id: &str, reason: String) -> Remote {
    observability::report_error(&format!(
        "distx: dropped the deferred announcement of tx {tx_id:?}: its coordinator could not be asked how the \
         transaction ends ({reason})"
    ));
    Remote::Dropped
}

fn stopped(tx_id: &str, reason: &str) {
    observability::report_error(&format!(
        "distx: dropped the deferred announcement of tx {tx_id:?}: the coordinator stopped answering before the \
         transaction ended ({reason}), so whether it committed is unknown"
    ));
}
