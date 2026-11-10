//! The in-process face of a distributed transaction (Go's
//! `service/tx/txscope`): which transaction a context belongs to, what has to
//! happen when it commits, and whether something inside it already decided
//! it must not.
//!
//! It exists for COMPOSITION: a business method that calls another one
//! through `registry.self_channel()` inside its own `distx::run` JOINS the
//! caller's transaction, and the callee's event waits for the caller's
//! commit.
//!
//! Work that must outlive its request — what a committed transaction owes
//! (events, `on_commit` hooks), a rollback — runs as a tracked task: a
//! request that is cancelled (its future dropped) does not take it along,
//! and the shutdown drain waits for it.

use std::{
    future::Future,
    panic::AssertUnwindSafe,
    pin::Pin,
    sync::{LazyLock, Mutex},
    time::Duration,
};

use futures::FutureExt as _;
use tokio_util::task::TaskTracker;

use crate::{ctx::Ctx, observability};

/// A piece of work parked on a transaction; `Err` is reported, never raised.
pub(crate) type Hook = Pin<Box<dyn Future<Output = Result<(), String>> + Send + 'static>>;

/// What [`crate::on_commit`] (and the event middleware) did with the work.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Outcome {
    /// The context belongs to no transaction: the work runs now.
    NoScope,
    /// It runs when the root commits, never if it rolls back.
    Deferred,
    /// The transaction committed first: the work runs now.
    AlreadyCommitted,
    /// The transaction rolled back first: the work does not run.
    Discarded,
}

#[derive(Default)]
struct State {
    hooks: Vec<Hook>,
    rollback_only: bool,
    ended: bool,
    committed: bool,
}

/// One root transaction as seen from the process that opened it.
#[derive(Default)]
pub(crate) struct Scope {
    st: Mutex<State>,
}

impl Scope {
    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.st.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// A joined part failed and rolled back. Without savepoints its writes
    /// cannot be undone alone, so the root must not commit.
    pub(crate) fn mark_rollback_only(&self) {
        self.lock().rollback_only = true;
    }

    pub(crate) fn rollback_only(&self) -> bool {
        self.lock().rollback_only
    }

    pub(crate) fn pending(&self) -> usize {
        self.lock().hooks.len()
    }

    /// Whether the scope has not ended: a begin on it JOINS.
    pub(crate) fn active(&self) -> bool {
        !self.lock().ended
    }

    /// Park `hook`; when the scope has ended it is handed back with what
    /// happened.
    pub(crate) fn park(&self, hook: Hook) -> (Outcome, Option<Hook>) {
        let mut st = self.lock();
        if st.ended {
            let out = if st.committed {
                Outcome::AlreadyCommitted
            } else {
                Outcome::Discarded
            };
            return (out, Some(hook));
        }
        st.hooks.push(hook);
        (Outcome::Deferred, None)
    }

    /// End the scope as committed and run the parked work in order. An error
    /// or a panic in one piece does not stop the rest and does not escape:
    /// the transaction has committed. What failed is returned for the caller to
    /// report.
    pub(crate) async fn run_committed(&self) -> Vec<String> {
        let hooks = {
            let mut st = self.lock();
            if st.ended {
                return Vec::new();
            }
            st.ended = true;
            st.committed = true;
            std::mem::take(&mut st.hooks)
        };
        let mut failures = Vec::new();
        for h in hooks {
            if let Err(e) = run_hook(h).await {
                failures.push(e);
            }
        }
        failures
    }

    /// End the scope as rolled back and drop the parked work.
    pub(crate) fn discarded(&self) {
        let mut st = self.lock();
        st.ended = true;
        st.hooks.clear();
    }
}

/// Run one piece of parked work to its end: its `Err`, or the text of a
/// panic it raised, is returned for the caller to report — a panic never
/// unwinds into the code that runs the work (a commit that landed, the
/// rest of the hooks).
pub(crate) async fn run_hook(hook: Hook) -> Result<(), String> {
    match AssertUnwindSafe(hook).catch_unwind().await {
        Ok(r) => r,
        Err(p) => Err(format!(
            "panicked: {}",
            crate::server::panic_text(p.as_ref())
        )),
    }
}

/// Park `hook` until the transaction `ctx` belongs to commits. The hook is
/// handed back when it must run now (no transaction, or one committed
/// already) or never (rolled back).
pub(crate) fn after_commit(ctx: &Ctx, hook: Hook) -> (Outcome, Option<Hook>) {
    match ctx.tx() {
        None => (Outcome::NoScope, Some(hook)),
        Some(root) => root.scope.park(hook),
    }
}

// --- work that must outlive the request ----------------------------------------

static TRACKER: LazyLock<TaskTracker> = LazyLock::new(TaskTracker::new);

/// Run `work` to completion as a tracked task and wait for it: a caller
/// whose future is dropped (its request was cancelled) does not stop it, and
/// [`drain`] waits for it. A panic in it is reported, and answered as an
/// error string.
pub(crate) async fn shielded<T: Send + 'static>(
    work: impl Future<Output = T> + Send + 'static,
) -> Result<T, String> {
    match TRACKER.spawn(work).await {
        Ok(v) => Ok(v),
        Err(e) => {
            let msg = format!("txscope: work run after its request failed: {e}");
            observability::report_error(&msg);
            Err(msg)
        }
    }
}

/// Start `work` as a tracked task and do not wait for it.
pub(crate) fn spawn_tracked(work: impl Future<Output = ()> + Send + 'static) {
    TRACKER.spawn(work);
}

/// How many tracked pieces of work are still running.
pub(crate) fn pending_count() -> usize {
    TRACKER.len()
}

/// Wait until no tracked work is left running — work started while waiting
/// included — or until `timeout` passes. True when none is left.
pub(crate) async fn drain(timeout: Duration) -> bool {
    TRACKER.close();
    let done = tokio::time::timeout(timeout, TRACKER.wait()).await.is_ok();
    TRACKER.reopen();
    done
}
