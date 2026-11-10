"""Distributed transactions — the Python twin of sdk/go/service/tx/distx.

The transaction lives in the storage server (``W17DistributedTransaction``
Begin / Commit / Rollback). Every storage call that carries ``w17-tx-id`` adopts
it; ``w17-conn-id``, when the storage proxy minted one in Begin's response
headers, pins the call to the replica that holds it. This module is the client
half, with Go's semantics:

- ``run(ctx, client, begin_request, fn)`` — begin, call ``fn(tx_ctx)``, commit
  when it returns, roll back when it raises (any exception, cancellation
  included, which keeps propagating).
- ``begin`` → ``TxHandle.commit`` / ``rollback`` for the manual form.
- NESTED: a begin on a context that already belongs to an open transaction
  JOINS it — no RPC; its commit is a no-op and its rollback marks the root
  rollback-only. A join needs the same connection and an unspecified or equal
  isolation, or it is ``NestedMismatchError``.
- The root's commit after a joined rollback rolls back and raises
  ``RollbackOnlyError``.
- A second commit/rollback on a finished root handle raises
  ``HandleFinishedError`` without an RPC.
- A commit that fails in transport is probed with a detached rollback: if the
  rollback succeeds the commit did not take effect (an ordinary failure); if not,
  ``CommitOutcomeUnknownError`` — the writes may have landed, and the work
  parked for the commit is NOT run.
- Rollback is detached from the caller's cancellation/deadline; a done context
  refuses to commit (``run``).
- ``Aborted`` from storage is raised to the caller, who retries the unit of work.

Work parked with ``on_commit`` (and every event emitted inside the transaction)
runs after the root commit, in order, and is dropped on rollback.
"""

from __future__ import annotations

import asyncio
import inspect
from collections.abc import Awaitable, Callable
from typing import Any, Protocol, TypeVar

from grpclib.const import Status
from grpclib.exceptions import GRPCError, StreamTerminatedError

from . import txscope
from .context import Context, Metadata
from .descriptors import default_pool
from .observability import report_error

__all__ = [
    "TX_ID_HEADER",
    "CONN_ID_HEADER",
    "DistxError",
    "RollbackOnlyError",
    "NestedMismatchError",
    "HandleFinishedError",
    "CommitOutcomeUnknownError",
    "ErrRollbackOnly",
    "ErrNestedMismatch",
    "ErrHandleFinished",
    "ErrCommitOutcomeUnknown",
    "TxClient",
    "TxHandle",
    "BeginRequest",
    "Isolation",
    "begin_request",
    "begin",
    "run",
    "on_commit",
    "OUTCOME_PROBE_TIMEOUT",
]

# txregistry.HeaderName / distx.ConnIDHeader.
TX_ID_HEADER = "w17-tx-id"
CONN_ID_HEADER = "w17-conn-id"

# Bounds the rollback a commit sends to settle an ambiguous outcome.
OUTCOME_PROBE_TIMEOUT = 10.0

T = TypeVar("T")


def _msg(name: str):
    return default_pool().message(f"w17.common.distx.{name}")


def BeginRequest(**kw: Any):  # noqa: N802 — a message constructor
    """``w17.common.distx.BeginRequest(connection_name=, tx_timeout_ms=, isolation=)``."""
    return _msg("BeginRequest")(**kw)


class Isolation:
    """``w17.common.distx.Isolation`` values."""

    UNSPECIFIED = 0
    READ_UNCOMMITTED = 1
    READ_COMMITTED = 2
    REPEATABLE_READ = 3
    SERIALIZABLE = 4


def begin_request(connection_name: str = "", tx_timeout_ms: int = 0, isolation: int = Isolation.UNSPECIFIED):
    """The ``BeginRequest`` for ``begin`` / ``run``. ``connection_name`` is the
    connection's name in the lock (``notes-postgres``): the storage tier's
    coordinator opens the transaction on it and refuses a name it does not
    serve, the empty one included (``InvalidArgument``, "unknown connection")."""
    return BeginRequest(connection_name=connection_name, tx_timeout_ms=tx_timeout_ms, isolation=isolation)


# --- errors ----------------------------------------------------------------


class DistxError(Exception):
    """Base of the four transaction errors."""


class RollbackOnlyError(DistxError):
    """A JOINED part of this transaction rolled back, so the root rolled the
    whole transaction back instead of committing it."""

    def __init__(self, detail: str = "") -> None:
        super().__init__(
            "distx: a nested part of this transaction rolled back, so the transaction was rolled back instead of committed"
            + (f"; {detail}" if detail else "")
        )


class NestedMismatchError(DistxError):
    """A begin found a transaction open on ctx but cannot JOIN it: another
    connection, or another isolation asked for explicitly. Refused rather than
    opened as a second transaction, which would not be atomic with the first
    (or could deadlock against its locks)."""


class HandleFinishedError(DistxError):
    """A second commit or rollback on a root handle whose transaction already
    ended through it."""


class CommitOutcomeUnknownError(DistxError):
    """The commit call failed in transport and the transaction could no longer
    be rolled back: the commit may have landed. Do not assume either outcome
    (retrying a non-idempotent write could apply it twice)."""


# Go's sentinel names, for code that reads like the Go guide.
ErrRollbackOnly = RollbackOnlyError
ErrNestedMismatch = NestedMismatchError
ErrHandleFinished = HandleFinishedError
ErrCommitOutcomeUnknown = CommitOutcomeUnknownError


# --- client protocol -------------------------------------------------------


class TxClient(Protocol):
    """The coordinator client distx drives. ``Clients.distributed_transaction()``
    returns one; a test may fake it. ``begin`` returns the response and the
    response's initial metadata (where the proxy puts ``w17-conn-id``)."""

    async def begin(self, ctx: Context, req: Any) -> tuple[Any, Metadata]: ...

    async def commit(self, ctx: Context, req: Any) -> Any: ...

    async def rollback(self, ctx: Context, req: Any) -> Any: ...


_ROOT = object()


def _root_from(ctx: Context) -> TxHandle | None:
    return ctx.value(_ROOT)


class TxHandle:
    """One distributed transaction opened by ``begin``. Single use: one commit
    OR one rollback; a failed attempt does not use it up."""

    def __init__(
        self,
        client: TxClient,
        tx_id: str,
        conn_id: str,
        connection: str,
        isolation: int,
        scope: txscope.Scope | None,
        joined: bool = False,
    ) -> None:
        self._client = client
        self._tx_id = tx_id
        self._conn_id = conn_id
        self._connection = connection
        self._isolation = isolation
        self._scope = scope
        self._joined = joined
        self._ended = ""
        self._lock = asyncio.Lock()

    @property
    def tx_id(self) -> str:
        return self._tx_id

    @property
    def conn_id(self) -> str:
        """The proxy routing token, "" when no proxy minted one."""
        return self._conn_id

    @property
    def joined(self) -> bool:
        return self._joined

    def attach(self, ctx: Context) -> Context:
        """ctx with this transaction's routing headers SET (never appended —
        an appended second ``w17-tx-id`` is adopted by nobody; storage adopts
        the first). ``w17-conn-id`` is removed when there is none."""
        out = ctx.outgoing.with_set(TX_ID_HEADER, self._tx_id)
        out = out.with_set(CONN_ID_HEADER, self._conn_id) if self._conn_id else out.with_set(CONN_ID_HEADER)
        return ctx.with_outgoing(out)

    def _claim(self) -> None:
        if self._ended:
            raise HandleFinishedError(
                f"distx: this transaction handle was already used: transaction {self._tx_id} was already {self._ended}"
            )

    async def commit(self, ctx: Context) -> None:
        """Commit; a joined handle commits nothing (the root does).

        Runs the parked work after a successful commit (failures reported,
        never raised); drops it otherwise."""
        if self._joined:
            return
        async with self._lock:
            self._claim()
            if self._scope is not None and self._scope.rollback_only:
                self._scope.discarded()
                try:
                    await self._client.rollback(self.attach(ctx.without_cancel()), _msg("RollbackRequest")(tx_id=self._tx_id))
                except Exception as rerr:
                    e = RollbackOnlyError(f"the rollback also failed: {rerr}")
                    raise e from rerr
                self._ended = "rolled back"
                raise RollbackOnlyError()
            err: BaseException | None = None
            try:
                await self._client.commit(self.attach(ctx), _msg("CommitRequest")(tx_id=self._tx_id))
            except asyncio.CancelledError:
                # The caller is being cancelled mid-commit: settle the outcome
                # on a detached context, then let the cancellation through.
                # Whichever way it settles, nothing parked may run: either
                # the transaction rolled back, or its outcome is unknown.
                # Used up BEFORE the settle is awaited: a second cancellation
                # during it must not leave a handle that commits again. A
                # settle whose rollback succeeds turns this into "rolled back".
                self._ended = "left with its outcome unknown"
                try:
                    await txscope.shielded(self._settle(ctx, None))
                finally:
                    if self._scope is not None:
                        self._scope.discarded()
                raise
            except Exception as e:
                err = e
                if _outcome_unknown(e):
                    err = await self._settle(ctx, e)
            if err is None:
                self._ended = "committed"
        if self._scope is None:
            if err is not None:
                raise err
            return
        if err is not None:
            self._scope.discarded()
            raise err
        # The transaction IS committed from here on. The parked work (the
        # events of the writes among it) runs to its end even when the caller
        # is cancelled now — a client hang-up must not lose them — and every
        # failure is reported, never raised into a commit that landed.
        scope, detached, tx_id = self._scope, ctx.without_cancel(), self._tx_id

        async def run_parked() -> None:
            for failure in await scope.run_committed():
                report_error(detached, RuntimeError(
                    f"distx: work parked on committed transaction {tx_id} failed: {failure!r}"))

        await txscope.shielded(run_parked())

    async def _settle(self, ctx: Context, commit_err: BaseException | None) -> BaseException | None:
        """After a commit with an unknown outcome: roll back, detached. Success
        means the transaction was still open, so the commit never took effect;
        failure means it is gone and the outcome is unknown."""
        probe = ctx.without_cancel().with_timeout(OUTCOME_PROBE_TIMEOUT)
        try:
            await self._client.rollback(self.attach(probe), _msg("RollbackRequest")(tx_id=self._tx_id))
        except Exception as rerr:
            why = repr(commit_err) if commit_err is not None else "the caller was cancelled mid-commit"
            pending = self._scope.pending() if self._scope is not None else 0
            report_error(ctx.without_cancel(), RuntimeError(
                f"distx: the outcome of committing transaction {self._tx_id} is unknown — the commit call failed "
                f"({why}) and the transaction could no longer be rolled back ({rerr!r}); its writes may have "
                f"landed, and {pending} piece(s) of work waiting for the commit were not run"))
            if self._scope is not None:
                self._scope.discarded()
            e = CommitOutcomeUnknownError(
                f"distx: the commit's outcome is unknown: transaction {self._tx_id}: {why}")
            e.__cause__ = commit_err
            return e
        self._ended = "rolled back"
        if commit_err is None:
            return None
        commit_err.add_note(
            f"distx: the commit of transaction {self._tx_id} did not take effect and it was rolled back")
        return commit_err

    async def rollback(self, ctx: Context) -> None:
        """Roll back. A joined handle marks the root rollback-only instead.

        Outlives ctx's cancellation and deadline: the commonest reason to roll
        back is that the caller is gone, and a call refused on a done context
        would leave the transaction held until the server times it out."""
        if self._joined:
            if self._scope is not None:
                self._scope.mark_rollback_only()
            return
        async with self._lock:
            self._claim()
            if self._scope is not None:
                self._scope.discarded()
            await self._client.rollback(self.attach(ctx.without_cancel()), _msg("RollbackRequest")(tx_id=self._tx_id))
            self._ended = "rolled back"


def _outcome_unknown(e: BaseException) -> bool:
    """A commit failure that does not say whether the server committed: it
    failed in transport or was abandoned rather than answered."""
    if isinstance(e, GRPCError):
        return e.status in (Status.UNAVAILABLE, Status.DEADLINE_EXCEEDED, Status.CANCELLED)
    return isinstance(e, (TimeoutError, ConnectionError, StreamTerminatedError, OSError))


async def begin(ctx: Context, client: TxClient, req: Any = None) -> tuple[TxHandle, Context]:
    """Open (or JOIN) a distributed transaction.

    Returns the handle and a context carrying ``w17-tx-id`` (and
    ``w17-conn-id`` when the proxy supplied one): pass that context to every
    storage call that belongs to the transaction."""
    if ctx.value(_BIDI) is True:
        raise GRPCError(
            Status.FAILED_PRECONDITION,
            "distx: a bidirectional-streaming method cannot run a transaction (as in Go): its messages "
            "interleave with the caller's, and nothing would bound the transaction")
    req = req if req is not None else BeginRequest()
    root = _root_from(ctx)
    if root is not None and txscope.active(ctx):
        return _join(ctx, root, req)
    resp, header = await client.begin(ctx, req)
    scoped, scope = txscope.new(ctx)
    conn_id = next((v for v in header.get_all(CONN_ID_HEADER) if v), "")
    h = TxHandle(client, resp.tx_id, conn_id, req.connection_name, req.isolation, scope)
    return h, h.attach(scoped.with_value(_ROOT, h))


_BIDI = object()


def _mark_bidi(ctx: Context) -> Context:
    """The context a bidirectional-streaming method is served with (the
    server sets it): begin refuses a transaction on it."""
    return ctx.with_value(_BIDI, True)


def _join(ctx: Context, root: TxHandle, req: Any) -> tuple[TxHandle, Context]:
    if req.connection_name != root._connection:
        raise NestedMismatchError(
            f"distx: cannot join the transaction already open on this context: it is on connection "
            f"{root._connection!r} and this begin asks for {req.connection_name!r} — a transaction on another "
            "connection would not be atomic with it")
    if req.isolation != Isolation.UNSPECIFIED and req.isolation != root._isolation:
        raise NestedMismatchError(
            f"distx: cannot join the transaction already open on this context: it runs at isolation "
            f"{root._isolation} and this begin asks for {req.isolation} — a running transaction cannot change level")
    h = TxHandle(root._client, root._tx_id, root._conn_id, root._connection, root._isolation, root._scope, joined=True)
    # SET the root's routing headers even when ctx already shows them.
    return h, h.attach(ctx)


async def run(
    ctx: Context,
    client: TxClient,
    req: Any,
    fn: Callable[[Context], Awaitable[T]],
) -> T:
    """Run ``fn`` inside one distributed transaction and return its result.

    Commit when fn returns; roll back (detached) when it raises — the
    exception keeps propagating, with a note when the rollback failed too. A
    caller whose context is done gets no commit. A failed commit is raised
    as it is (with a note naming the commit), and fn's work is lost.

    fn must use the context it receives for every write of the transaction."""
    if client is None:
        raise ValueError("distx.run: nil transaction client")
    if fn is None:
        raise ValueError("distx.run: nil function — an empty transaction is a mistake, not a no-op")
    try:
        handle, tx_ctx = await begin(ctx, client, req)
    except Exception as e:
        e.add_note("distx.run: begin")
        raise
    detached = ctx.without_cancel()
    try:
        result = fn(tx_ctx)
        if inspect.isawaitable(result):
            result = await result
    except BaseException as ferr:
        try:
            await asyncio.shield(handle.rollback(detached))
        except Exception as rerr:  # noqa: BLE001
            ferr.add_note(f"distx.run: rollback also failed: {rerr!r}")
        raise
    if ctx.done():
        # The refusal is what the caller acts on; a failed rollback is named
        # in it, as Go's Run does — the transaction may be left open until
        # the server times it out.
        also = ""
        try:
            await asyncio.shield(handle.rollback(detached))
        except Exception as rerr:  # noqa: BLE001
            also = f" (rollback also failed: {rerr!r})"
        raise DistxError(f"distx.run: not committing, the caller's context is done: {ctx.err()}{also}")
    try:
        await handle.commit(ctx)
    except Exception as cerr:
        cerr.add_note("distx.run: commit")
        raise
    return result


async def on_commit(ctx: Context, fn: Callable[[], Any]) -> txscope.Outcome:
    """Run ``fn`` once the transaction ctx belongs to commits; never if it
    rolls back; at once outside a transaction. ``fn`` may be sync or async.

    Outside a transaction (or after its commit) fn runs now, before this
    returns, and an exception from it propagates — it is the caller's own
    code on the caller's own path. Inside one it is parked and the result is
    ``Outcome.DEFERRED``; a failure then is reported, never raised into the
    commit (the transaction has landed). Inside a nested ``run`` that JOINED
    an outer transaction, fn waits for the OUTER commit.

    The hook the event middleware itself uses, offered for any side effect
    that must not outrun the write it describes (an e-mail, an external call).
    Go's twin is ``distx.OnCommit``."""
    if fn is None:
        return txscope.Outcome.NO_SCOPE
    out = txscope.after_commit(ctx, fn)
    if out in (txscope.Outcome.NO_SCOPE, txscope.Outcome.ALREADY_COMMITTED):
        r = fn()
        if inspect.isawaitable(r):
            await r
    elif out is txscope.Outcome.DISCARDED:
        report_error(ctx, "distx: on_commit work dropped: it was registered after its transaction rolled back")
    return out
