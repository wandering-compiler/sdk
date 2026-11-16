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
  rollback succeeds the commit did not take effect (an ordinary failure). When
  the transaction is gone but the coordinator remembers how it ended (the
  ``ended`` detail on its NotFound), that settles it: success and the parked
  work runs when it committed, an ordinary failure when it did not. Only a
  coordinator that cannot say leaves ``CommitOutcomeUnknownError`` — the
  writes may have landed, and the work parked for the commit is NOT run.
- A commit refused with ``FAILED_PRECONDITION`` (a call inside the transaction
  still holds it) leaves the transaction open: the handle is retryable and the
  parked work waits for the retry. ``run`` rolls such a transaction back
  rather than leave it holding its locks until the orphan timeout.
- Rollback is detached from the caller's cancellation/deadline and bounded by
  ``ROLLBACK_WAIT`` of its own; a rollback of a transaction that already ended
  without committing is done, not a failure. A done context refuses to commit
  (``run``).
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
from .errors import error_infos
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
    "ROLLBACK_WAIT",
    "ENDED_DOMAIN",
    "ENDED_REASON",
    "ENDED_KEY",
]

# txregistry.HeaderName / distx.ConnIDHeader.
TX_ID_HEADER = "w17-tx-id"
CONN_ID_HEADER = "w17-conn-id"

# Bounds the rollback a commit sends to settle an ambiguous outcome.
OUTCOME_PROBE_TIMEOUT = 10.0

# Bounds every rollback sent on a context detached from its caller's
# (TxHandle.rollback, the rollback-only commit, run's rollback). Detaching
# drops the caller's deadline too, so without a bound a coordinator that
# accepted the call and stalls would hold the caller forever. The server dooms
# the transaction before it waits for a running handler's lease, so a rollback
# cut by this bound is still decided. Go's rollbackWait: the finish wait, equal
# to the auto-rollback interceptor's RollbackWait.
ROLLBACK_WAIT = 30.0

# The google.rpc.ErrorInfo the coordinator attaches to a NotFound for a
# transaction it remembers (its last 256 ids): metadata[ENDED_KEY] is
# "committed" or "not_committed". Go's distx.EndedDomain / EndedReason /
# EndedKey.
ENDED_DOMAIN = "w17.distx"
ENDED_REASON = "W17_TX_ENDED"
ENDED_KEY = "ended"
_COMMITTED = "committed"
_NOT_COMMITTED = "not_committed"

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
                    await self._client.rollback(self.attach(_detached_rollback_ctx(ctx)), _msg("RollbackRequest")(tx_id=self._tx_id))
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
                # The parked work runs only when the coordinator recorded the
                # commit as landed; otherwise the transaction rolled back or
                # its outcome is unknown, and it is dropped. Used up BEFORE the
                # settle is awaited: a second cancellation during it must not
                # leave a handle that commits again. The settle (and the
                # parked work it may run) finishes even then, tracked.
                self._ended = "left with its outcome unknown"
                await txscope.shielded(self._settle_cancelled(ctx))
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
            # Only an answer that ENDED the transaction drops its parked work.
            # A refusal that left it open (a call still holds it) is
            # retryable on this handle, and the retry's commit must still run
            # the work waiting for it.
            if not _left_open(err):
                self._scope.discarded()
            raise err
        # The transaction IS committed from here on. The parked work (the
        # events of the writes among it) runs to its end even when the caller
        # is cancelled now — a client hang-up must not lose them — and every
        # failure is reported, never raised into a commit that landed.
        await txscope.shielded(self._run_parked(ctx))

    async def _run_parked(self, ctx: Context) -> None:
        scope, detached, tx_id = self._scope, ctx.without_cancel(), self._tx_id
        if scope is None:
            return
        for failure in await scope.run_committed():
            report_error(detached, RuntimeError(
                f"distx: work parked on committed transaction {tx_id} failed: {failure!r}"))

    async def _settle_cancelled(self, ctx: Context) -> None:
        """The settle of a commit its caller cancelled, with what follows it:
        the parked work runs when the coordinator recorded the commit, and is
        dropped otherwise."""
        committed = False
        try:
            await self._settle(ctx, None)
            committed = self._ended == "committed"
        finally:
            if self._scope is not None and not committed:
                self._scope.discarded()
        if committed:
            await self._run_parked(ctx)

    async def _settle(self, ctx: Context, commit_err: BaseException | None) -> BaseException | None:
        """After a commit with an unknown outcome: roll back, detached. Success
        means the transaction was still open, so the commit never took effect.
        A NotFound carrying the coordinator's ended detail settles it from its
        record (committed: None and the handle reads "committed"; not
        committed: an ordinary failure). Any other failure means it is gone
        and the outcome is unknown."""
        probe = ctx.without_cancel().with_timeout(OUTCOME_PROBE_TIMEOUT)
        try:
            await self._client.rollback(self.attach(probe), _msg("RollbackRequest")(tx_id=self._tx_id))
        except Exception as rerr:
            ended = _ended_from(rerr)
            if ended == _COMMITTED:
                # The commit landed; only its reply was lost.
                self._ended = "committed"
                return None
            if ended == _NOT_COMMITTED:
                self._ended = "rolled back"
                if commit_err is None:
                    return None
                commit_err.add_note(f"distx: transaction {self._tx_id} did not commit ({rerr!r})")
                return commit_err
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
        would leave the transaction held until the server times it out. It is
        bounded by ``ROLLBACK_WAIT`` instead, so a stalled coordinator cannot
        hold the caller forever. A transaction that already ended without
        committing is rolled back as asked: no error."""
        if self._joined:
            if self._scope is not None:
                self._scope.mark_rollback_only()
            return
        async with self._lock:
            self._claim()
            if self._scope is not None:
                self._scope.discarded()
            try:
                await self._client.rollback(self.attach(_detached_rollback_ctx(ctx)), _msg("RollbackRequest")(tx_id=self._tx_id))
            except Exception as rerr:
                ended = _ended_from(rerr)
                if ended == _NOT_COMMITTED:
                    # Already ended without committing — the auto-rollback
                    # interceptor discarded it after a failed call, or its
                    # orphan bound ran out. That is what this rollback asked
                    # for, so it is not a failure.
                    self._ended = "rolled back"
                    return
                if ended == _COMMITTED:
                    self._ended = "committed"
                    raise DistxError(
                        f"distx: cannot roll back transaction {self._tx_id}, it was already committed") from rerr
                raise
            self._ended = "rolled back"


def _detached_rollback_ctx(ctx: Context) -> Context:
    """ctx without its cancellation and deadline (values and metadata kept),
    bounded by ``ROLLBACK_WAIT``."""
    return ctx.without_cancel().with_timeout(ROLLBACK_WAIT)


def _left_open(e: BaseException) -> bool:
    """A commit failure after which the transaction is still open: the
    coordinator refused to finish it because a call inside it still holds it
    past the finish wait. The handle is not used up; the caller retries, or
    rolls back (``run`` rolls back)."""
    return isinstance(e, GRPCError) and e.status == Status.FAILED_PRECONDITION


def _ended_from(e: BaseException) -> str:
    """The coordinator's record of how a transaction it no longer holds ended
    (``"committed"`` / ``"not_committed"``), read from the ErrorInfo on its
    NotFound; "" for any other error or a NotFound without it."""
    if not isinstance(e, GRPCError) or e.status != Status.NOT_FOUND:
        return ""
    for info in error_infos(e):
        if info.domain != ENDED_DOMAIN or info.reason != ENDED_REASON:
            continue
        ended = info.metadata.get(ENDED_KEY, "")
        if ended in (_COMMITTED, _NOT_COMMITTED):
            return ended
    return ""


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
    as it is (with a note naming the commit), and fn's work is lost; a commit
    refused because a call still held the transaction is rolled back first.

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
        # A refusal that left the transaction open (a call inside it still
        # held it past the finish wait) is the one commit failure after which
        # a rollback is still owed: run reports the failure, so it must not
        # leave the transaction holding its locks until the orphan timeout.
        # The server dooms it at once and rolls it back when the holder
        # releases. Every other answer already ended it.
        if _left_open(cerr):
            try:
                await asyncio.shield(handle.rollback(detached))
            except Exception as rerr:  # noqa: BLE001
                cerr.add_note(f"distx.run: rollback also failed: {rerr!r}")
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
