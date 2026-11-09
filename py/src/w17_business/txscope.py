"""The in-process face of a distributed transaction (Go's ``service/tx/txscope``).

Which transaction a context belongs to, what has to happen when it commits, and
whether something inside it already decided it must not. It exists for
COMPOSITION: a business method that calls another one through
``registry.self(...)`` inside its own ``distx.run`` must JOIN the caller's
transaction (not open a second one), and the callee's event must wait for the
caller's commit.

Single-threaded asyncio needs no lock: every state change below happens between
two awaits, so it is atomic with respect to other tasks.
"""

from __future__ import annotations

import asyncio
import enum
import inspect
from collections.abc import Awaitable, Callable
from typing import Any, TypeVar

from .context import Context

__all__ = ["Scope", "Outcome", "new", "scope_from", "active", "after_commit", "detach", "shielded", "drain", "cancel_pending"]

T = TypeVar("T")

_KEY = object()


class Outcome(enum.Enum):
    """What ``after_commit`` did with the work it was given."""

    NO_SCOPE = 0  # ctx belongs to no transaction; the caller runs the work now
    DEFERRED = 1  # runs when the root commits, never if it rolls back
    ALREADY_COMMITTED = 2  # the transaction committed first; run it now
    DISCARDED = 3  # the transaction rolled back first; it must not run


class Scope:
    """One root transaction as seen from the process that opened it."""

    __slots__ = ("_hooks", "_rollback_only", "ended", "committed")

    def __init__(self) -> None:
        self._hooks: list[Callable[[], Any]] = []
        self._rollback_only = False
        self.ended = False
        self.committed = False

    def mark_rollback_only(self) -> None:
        """A joined part failed and rolled back. Without savepoints its writes
        cannot be undone alone, so the root must not commit."""
        self._rollback_only = True

    @property
    def rollback_only(self) -> bool:
        return self._rollback_only

    def pending(self) -> int:
        return len(self._hooks)

    async def run_committed(self) -> list[BaseException]:
        """End the scope as committed and run the parked work in order.

        An exception in one piece does not stop the rest and does not escape:
        the transaction has already committed, and an exception unwinding into
        its caller would read as a failed commit. What failed is returned for
        the caller to report."""
        if self.ended:
            return []
        self.ended = self.committed = True
        hooks, self._hooks = self._hooks, []
        failures: list[BaseException] = []
        for i, fn in enumerate(hooks):
            try:
                r = fn()
                if inspect.isawaitable(r):
                    await r
            except BaseException as e:  # noqa: BLE001 — one hook (its own cancellation included) never stops the rest
                task = asyncio.current_task()
                if isinstance(e, asyncio.CancelledError) and task is not None and task.cancelling() > 0:
                    # The task running the hooks is itself being cancelled
                    # (the drain's deadline, asyncio.run's teardown): stop,
                    # say what did not run, and let the cancellation through —
                    # swallowing it would run the next hook, which may hang
                    # the exit forever.
                    from .observability import report_error

                    report_error(None, RuntimeError(
                        f"txscope: cancelled while running the work parked on a committed transaction; "
                        f"{len(hooks) - i} piece(s) of it did not run to the end"))
                    raise
                failures.append(e)
        return failures

    def discarded(self) -> None:
        """End the scope as rolled back and drop the parked work."""
        self.ended = True
        self._hooks = []

    def _park(self, fn: Callable[[], Any]) -> Outcome:
        if self.ended:
            return Outcome.ALREADY_COMMITTED if self.committed else Outcome.DISCARDED
        self._hooks.append(fn)
        return Outcome.DEFERRED


def new(ctx: Context) -> tuple[Context, Scope]:
    s = Scope()
    return ctx.with_value(_KEY, s), s


def scope_from(ctx: Context) -> Scope | None:
    return ctx.value(_KEY)


def active(ctx: Context) -> bool:
    """Whether ctx belongs to a scope that has not ended — when a new
    transaction JOINS instead of beginning."""
    s = scope_from(ctx)
    return s is not None and not s.ended


def detach(ctx: Context) -> Context:
    """ctx with its scope hidden (Go's ``txscope.Detach``)."""
    if scope_from(ctx) is None:
        return ctx
    return ctx.with_value(_KEY, None)


def after_commit(ctx: Context, fn: Callable[[], Any]) -> Outcome:
    """Park fn until the transaction ctx belongs to commits."""
    s = scope_from(ctx)
    if s is None or fn is None:
        return Outcome.NO_SCOPE
    return s._park(fn)


# --- work that must outlive the request ----------------------------------------
#
# Go runs the parked work on the goroutine that commits, and a goroutine is not
# interrupted when its client hangs up. An asyncio request task IS: grpclib
# cancels it, and a CancelledError raised into a hook after storage committed
# would skip that hook and every one after it — events of a write that landed,
# lost without a word. So that work runs in a task of its own, which the
# caller awaits through asyncio.shield (a cancelled caller stops waiting, the
# work does not stop), and which the drain waits for before the process exits.

_detached: set[asyncio.Task] = set()


def _reap(t: asyncio.Task) -> None:
    _detached.discard(t)
    if t.cancelled():
        return
    e = t.exception()
    if e is not None:
        from .observability import report_error

        report_error(None, RuntimeError(f"txscope: work run after its request failed: {e!r}"))


async def shielded(aw: Awaitable[T]) -> T:
    """Run aw to completion even when the awaiting task is cancelled.

    The caller sees its own cancellation as usual; aw keeps running, tracked,
    until it ends (``drain`` waits for it)."""
    t = asyncio.ensure_future(aw)
    _detached.add(t)
    t.add_done_callback(_reap)
    return await asyncio.shield(t)


def _running() -> list[asyncio.Task]:
    loop = asyncio.get_running_loop()
    return [t for t in _detached if not t.done() and t.get_loop() is loop]


async def drain(timeout: float) -> bool:
    """Wait until no work ``shielded`` started is left running — including
    work started while waiting (a handler cancelled late, then committing) —
    or until one overall deadline of timeout seconds passes. True when none
    is left."""
    loop = asyncio.get_running_loop()
    deadline = loop.time() + max(timeout, 0)
    while True:
        # Let work that is about to start (a shielded() not yet scheduled) do so.
        await asyncio.sleep(0)
        pending = _running()
        if not pending:
            return True
        remaining = deadline - loop.time()
        if remaining <= 0:
            return False
        await asyncio.wait(pending, timeout=remaining)


async def cancel_pending(timeout: float) -> int:
    """Cancel the work still running after the drain and wait for it, bounded
    by timeout. Returns how many pieces were cancelled."""
    pending = _running()
    for t in pending:
        t.cancel()
    if pending:
        await asyncio.wait(pending, timeout=max(timeout, 0.001))
    return len(pending)
