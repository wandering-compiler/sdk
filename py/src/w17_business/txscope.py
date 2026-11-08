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

import enum
import inspect
from collections.abc import Callable
from typing import Any

from .context import Context

__all__ = ["Scope", "Outcome", "new", "scope_from", "active", "after_commit", "detach"]

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
        for fn in hooks:
            try:
                r = fn()
                if inspect.isawaitable(r):
                    await r
            except Exception as e:  # noqa: BLE001 — reported, never re-raised
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
