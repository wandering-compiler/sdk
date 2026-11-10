//! The context a handler is given and passes on.
//!
//! Go's rule — "the context you were given is the one you pass on" — is what
//! keeps the caller's identity, deadline and transaction attached to every
//! call a handler makes. Rust has no `context.Context`, and an ambient
//! substitute (a task-local) loses its value in work started outside the
//! request without anyone noticing. So the context is an explicit, immutable
//! value: the server middleware puts one in every request's extensions
//! ([`ctx`] takes it out), every call a handler makes carries one
//! ([`Ctx::request`]), and every helper that changes it
//! ([`crate::paging::forward`], [`crate::principal::act_as`],
//! [`crate::distx::begin`]) returns a NEW one — as a Go `context.WithValue`.
//!
//! A context carries the INCOMING metadata of the request being served (what
//! the gateway stamped), the OUTGOING metadata the next call sends, the
//! deadline, a cancellation, and the transaction it belongs to.

use std::{
    fmt,
    sync::{Arc, Mutex, OnceLock},
    time::{Duration, Instant},
};

use tokio_util::sync::CancellationToken;
use tonic::Status;

use crate::{distx::Root, metadata::Metadata};

/// Why a context is done — Go's `ctx.Err()`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CtxError {
    /// "context canceled"
    Canceled,
    /// "context deadline exceeded"
    DeadlineExceeded,
}

impl CtxError {
    /// The status a call refused (or ended) for this reason answers with.
    pub fn status(self) -> Status {
        match self {
            CtxError::Canceled => Status::cancelled(self.to_string()),
            CtxError::DeadlineExceeded => Status::deadline_exceeded(self.to_string()),
        }
    }
}

impl fmt::Display for CtxError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            CtxError::Canceled => "context canceled",
            CtxError::DeadlineExceeded => "context deadline exceeded",
        })
    }
}

impl std::error::Error for CtxError {}

/// The response headers a method answers with, filled by the calls it makes
/// (the page position a forwarded storage call reported).
pub(crate) type HeaderSink = Arc<Mutex<Metadata>>;

#[derive(Clone)]
struct Inner {
    incoming: Metadata,
    outgoing: Metadata,
    deadline: Option<Instant>,
    cancel: CancellationToken,
    method: Arc<str>,
    tx: Option<Arc<Root>>,
    paging_forwarded: bool,
    response_headers: Option<HeaderSink>,
    trace: Option<opentelemetry::Context>,
    bidi: bool,
    /// When each cancellation of the chain from the root to this context was
    /// cancelled through the SDK (its own last): `err` keeps the first cause,
    /// as Go's `ctx.Err()` does, so it has to know whether the cancel or the
    /// deadline came first.
    clocks: Arc<Vec<CancelClock>>,
}

/// When a cancellation fired, recorded by whoever cancels through the SDK
/// ([`Ctx::cancel`], the server ending a call).
#[derive(Clone, Default)]
pub(crate) struct CancelClock(Arc<OnceLock<Instant>>);

impl CancelClock {
    fn mark(&self) {
        let _ = self.0.set(Instant::now());
    }

    fn at(&self) -> Option<Instant> {
        self.0.get().copied()
    }
}

/// Cancels a served call's context when dropped (the call ended, or the
/// caller went away), recording when.
pub(crate) struct CancelOnDrop {
    token: CancellationToken,
    clock: CancelClock,
}

impl CancelOnDrop {
    pub(crate) fn new(token: CancellationToken, clock: CancelClock) -> Self {
        Self { token, clock }
    }
}

impl Drop for CancelOnDrop {
    fn drop(&mut self) {
        if !self.token.is_cancelled() {
            self.clock.mark();
        }
        self.token.cancel();
    }
}

/// An immutable request context (cheap to clone). See the module doc.
#[derive(Clone)]
pub struct Ctx(Arc<Inner>);

impl Default for Ctx {
    fn default() -> Self {
        Self::background()
    }
}

impl Ctx {
    /// An empty context: no metadata, no deadline, never cancelled (Go's
    /// `context.Background()`).
    pub fn background() -> Self {
        Ctx(Arc::new(Inner {
            incoming: Metadata::new(),
            outgoing: Metadata::new(),
            deadline: None,
            cancel: CancellationToken::new(),
            method: Arc::from(""),
            tx: None,
            paging_forwarded: false,
            response_headers: None,
            trace: None,
            bidi: false,
            clocks: Arc::new(vec![CancelClock::default()]),
        }))
    }

    fn with(&self, f: impl FnOnce(&mut Inner)) -> Self {
        let mut inner = (*self.0).clone();
        f(&mut inner);
        Ctx(Arc::new(inner))
    }

    /// The metadata of the request being served.
    pub fn incoming(&self) -> &Metadata {
        &self.0.incoming
    }

    /// The metadata the next call sends (besides the relayed principal).
    pub fn outgoing(&self) -> &Metadata {
        &self.0.outgoing
    }

    /// The full gRPC method being served (`/pkg.Svc/Method`), "" outside a
    /// handler.
    pub fn method(&self) -> &str {
        &self.0.method
    }

    /// The deadline, if any.
    pub fn deadline(&self) -> Option<Instant> {
        self.0.deadline
    }

    /// The time left before the deadline (zero once it passed), `None` when
    /// there is none.
    pub fn time_remaining(&self) -> Option<Duration> {
        self.0
            .deadline
            .map(|d| d.saturating_duration_since(Instant::now()))
    }

    /// A copy whose incoming metadata is `md`.
    pub fn with_incoming(&self, md: Metadata) -> Self {
        self.with(|i| i.incoming = md)
    }

    /// A copy whose outgoing metadata is `md`.
    pub fn with_outgoing(&self, md: Metadata) -> Self {
        self.with(|i| i.outgoing = md)
    }

    /// A child whose deadline is the earlier of this one's and now + `d`;
    /// cancelling this context cancels it.
    pub fn with_timeout(&self, d: Duration) -> Self {
        let mut deadline = Instant::now() + d;
        if let Some(own) = self.0.deadline {
            deadline = deadline.min(own);
        }
        let token = self.0.cancel.child_token();
        let clocks = self.child_clocks();
        self.with(|i| {
            i.deadline = Some(deadline);
            i.cancel = token;
            i.clocks = clocks;
        })
    }

    /// A child that [`Ctx::cancel`] ends without ending this one.
    pub fn with_cancel(&self) -> Self {
        let token = self.0.cancel.child_token();
        let clocks = self.child_clocks();
        self.with(|i| {
            i.cancel = token;
            i.clocks = clocks;
        })
    }

    /// Go's `context.WithoutCancel`: the same metadata and values, no
    /// deadline, never done. For work that must outlive the request — a
    /// rollback, a commit-outcome probe, an event published after commit.
    pub fn without_cancel(&self) -> Self {
        self.with(|i| {
            i.deadline = None;
            i.cancel = CancellationToken::new();
            i.clocks = Arc::new(vec![CancelClock::default()]);
        })
    }

    fn child_clocks(&self) -> Arc<Vec<CancelClock>> {
        let mut clocks = (*self.0.clocks).clone();
        clocks.push(CancelClock::default());
        Arc::new(clocks)
    }

    /// Cancel this context and every one derived from it.
    pub fn cancel(&self) {
        if !self.0.cancel.is_cancelled()
            && let Some(own) = self.0.clocks.last()
        {
            own.mark();
        }
        self.0.cancel.cancel();
    }

    /// Why the context is done, `None` while it is not (Go's `ctx.Err()`).
    ///
    /// Whichever came first, as Go keeps the first: a cancel before the
    /// deadline stays [`CtxError::Canceled`] after the deadline passes, and a
    /// deadline that had passed when the context was cancelled is the reason.
    pub fn err(&self) -> Option<CtxError> {
        let past = self.0.deadline.is_some_and(|d| Instant::now() >= d);
        if self.0.cancel.is_cancelled() {
            let at = self.0.clocks.iter().filter_map(CancelClock::at).min();
            return Some(match (at, self.0.deadline) {
                (Some(at), Some(d)) if d <= at => CtxError::DeadlineExceeded,
                (Some(_), _) => CtxError::Canceled,
                // cancelled where no clock saw it: the deadline, if passed
                (None, _) if past => CtxError::DeadlineExceeded,
                (None, _) => CtxError::Canceled,
            });
        }
        if past {
            return Some(CtxError::DeadlineExceeded);
        }
        None
    }

    /// True once cancelled or past the deadline.
    pub fn is_done(&self) -> bool {
        self.err().is_some()
    }

    /// Resolves once the context is done, with the reason.
    pub async fn done(&self) -> CtxError {
        match self.0.deadline {
            Some(d) => tokio::select! {
                _ = self.0.cancel.cancelled() => self.err().unwrap_or(CtxError::Canceled),
                _ = tokio::time::sleep_until(d.into()) => CtxError::DeadlineExceeded,
            },
            None => {
                self.0.cancel.cancelled().await;
                CtxError::Canceled
            }
        }
    }

    /// A `tonic::Request` carrying `msg` and this context: what every call a
    /// handler makes is built with. The SDK's channels read the context from
    /// the request and refuse one without it.
    pub fn request<T>(&self, msg: T) -> tonic::Request<T> {
        let mut req = tonic::Request::new(msg);
        req.extensions_mut().insert(self.clone());
        req
    }

    // --- crate-internal values ------------------------------------------------

    pub(crate) fn served(
        incoming: Metadata,
        deadline: Option<Instant>,
        cancel: CancellationToken,
        clock: CancelClock,
        method: &str,
        sink: Option<HeaderSink>,
        bidi: bool,
    ) -> Self {
        Ctx(Arc::new(Inner {
            incoming,
            outgoing: Metadata::new(),
            deadline,
            cancel,
            method: Arc::from(method),
            tx: None,
            paging_forwarded: false,
            response_headers: sink,
            trace: None,
            bidi,
            clocks: Arc::new(vec![clock]),
        }))
    }

    pub(crate) fn tx(&self) -> Option<&Arc<Root>> {
        self.0.tx.as_ref()
    }

    pub(crate) fn with_tx(&self, tx: Option<Arc<Root>>) -> Self {
        self.with(|i| i.tx = tx)
    }

    pub(crate) fn paging_forwarded(&self) -> bool {
        self.0.paging_forwarded
    }

    pub(crate) fn with_paging_forwarded(&self) -> Self {
        self.with(|i| i.paging_forwarded = true)
    }

    pub(crate) fn response_headers(&self) -> Option<&HeaderSink> {
        self.0.response_headers.as_ref()
    }

    pub(crate) fn trace(&self) -> Option<&opentelemetry::Context> {
        self.0.trace.as_ref()
    }

    pub(crate) fn with_trace(&self, trace: opentelemetry::Context) -> Self {
        self.with(|i| i.trace = Some(trace))
    }

    pub(crate) fn is_bidi(&self) -> bool {
        self.0.bidi
    }
}

impl fmt::Debug for Ctx {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Ctx")
            .field("method", &&*self.0.method)
            .field("incoming", &self.0.incoming)
            .field("outgoing", &self.0.outgoing)
            .field("deadline", &self.0.deadline)
            .field("in_transaction", &self.0.tx.is_some())
            .finish()
    }
}

/// The context the server middleware put on `req`: the one a handler passes
/// on. A request with none (a handler called directly, not through the
/// served router) is `INTERNAL`, naming the cause.
pub fn ctx<T>(req: &tonic::Request<T>) -> Result<Ctx, Status> {
    req.extensions().get::<Ctx>().cloned().ok_or_else(|| {
        Status::internal(
            "w17_business::ctx: the request carries no context — it did not come through the served router \
             (call your own methods through registry.self_channel(), never directly)",
        )
    })
}
