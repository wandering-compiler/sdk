"""A business method reached OVER THE WIRE inside another process's transaction.

The request carries the caller's ``w17-tx-id`` and no in-process scope: the
transaction was begun in the caller's process, so nothing here hears how it
ends unless the coordinator is asked (``W17TransactionOutcome.Await``). The
method's events must wait for that commit exactly as they would inside one
process: published once on the commit, never on a rollback, and not twice
when a commit refused while a call still held the transaction is retried.

Real grpclib servers on loopback: a coordinator stand-in serving Await from a
script, and the business tier serving CreateNote (whose emit the manifest
declares) without a transaction of its own — what a method does when it takes
part in its caller's."""

import asyncio
import base64

import pytest
from grpclib.const import Status
from grpclib.exceptions import GRPCError
from grpclib.server import Server

from test_server import EMIT, NoteService
from w17_business import distx, events, txscope
from w17_business.client import Conn, ServiceClient, TxOutcomeClient
from w17_business.context import Context
from w17_business.errors import StatusDetailsCodec
from w17_business.server import Registry

PENDING, COMMITTED, NOT_COMMITTED = 0, 1, 2


class Hold:
    """A script step: send nothing until the test releases it (an open
    transaction, or a commit refused while a call still holds it)."""

    def __init__(self) -> None:
        self.ev = asyncio.Event()

    def release(self) -> None:
        self.ev.set()


class FakeOutcome:
    """W17TransactionOutcome: per tx id a script of outcomes, holds and
    errors; an id with no script answers NotFound, as the coordinator does
    for one it neither holds nor remembers."""

    def __init__(self) -> None:
        self.scripts: dict[str, list] = {}
        self.calls: list[tuple[str, dict]] = []

    async def Await(self, ctx, req):
        self.calls.append((req.tx_id, {k: v for k, v in ctx.incoming.items()}))
        script = self.scripts.get(req.tx_id)
        if script is None:
            raise GRPCError(Status.NOT_FOUND, f"distx: unknown transaction {req.tx_id}")
        for step in script:
            if isinstance(step, Hold):
                await step.ev.wait()
            elif isinstance(step, BaseException):
                raise step
            else:
                yield distx._msg("AwaitResponse")(outcome=step)


class Remote(NoteService):
    """CreateNote as a method taking part in its caller's transaction: no
    distx.run of its own (its writes would forward the caller's headers)."""

    async def CreateNote(self, ctx, req):
        return self.M("Note")(id=7, text=req.text)


@pytest.fixture
def reported(monkeypatch):
    out: list[str] = []
    monkeypatch.setattr(distx, "report_error", lambda ctx, e: out.append(str(e)))
    return out


class OldCoordinator:
    """A coordinator from before the outcome service: grpc-go answers its
    unknown service UNIMPLEMENTED. (grpclib's own answer to an unknown method
    omits the content-type, which its client then reads as UNKNOWN — a
    stand-in artefact, so the stand-in answers as grpc-go does.)"""

    async def Await(self, ctx, req):
        raise GRPCError(Status.UNIMPLEMENTED, "unknown service w17.common.distx.W17TransactionOutcome")
        yield  # pragma: no cover — makes this a server-streaming handler


async def _stack(pool, *, serve_outcome=True, wire_outcome=True):
    fake = FakeOutcome()
    creg = Registry(pool)
    creg.register("w17.common.distx.W17TransactionOutcome", fake if serve_outcome else OldCoordinator())
    csrv = Server(creg.served, status_details_codec=StatusDetailsCodec())
    await csrv.start("127.0.0.1", 0)
    cconn = Conn(f"127.0.0.1:{csrv._server.sockets[0].getsockname()[1]}", tls=False)

    pub = events.MemoryPublisher()
    breg = Registry(pool, (EMIT,), pub)
    if wire_outcome:
        breg.tx_outcome = TxOutcomeClient(cconn)
    breg.register("conformance.v1.NoteService", Remote(breg, None, None))
    bsrv = Server(breg.served, status_details_codec=StatusDetailsCodec())
    await bsrv.start("127.0.0.1", 0)
    bconn = Conn(f"127.0.0.1:{bsrv._server.sockets[0].getsockname()[1]}", tls=False)

    class S:
        pass

    s = S()
    s.fake, s.pub, s.registry = fake, pub, breg
    s.client = ServiceClient(bconn, "conformance.v1.NoteService", pool)

    async def close():
        bconn.close()
        cconn.close()
        bsrv.close()
        csrv.close()
        await bsrv.wait_closed()
        await csrv.wait_closed()

    s.close = close
    return s


@pytest.fixture
async def stack(pool):
    s = await _stack(pool)
    yield s
    # The waits a test left open end with the stack (their streams break).
    await s.close()
    await txscope.cancel_pending(1.0)


def _caller_ctx(msg, tx="tx-1", conn="replica-2"):
    """What a caller in another process sends: its transaction, the replica
    holding it, its principal, and a header of its own."""
    env = base64.b64encode(msg("AuthResp")(user_id="u-1").SerializeToString()).decode()
    out = {"w17-tx-id": tx, "x-w17-user": env, "x-custom": "c"}
    if conn:
        out["w17-conn-id"] = conn
    return Context(outgoing=out)


async def _call(s, msg, **kw):
    return await s.client.CreateNote(_caller_ctx(msg, **kw), msg("CreateNoteRequest")(text="r"))


async def _settle(pub, n, timeout=3.0):
    """Wait for n publishes, then a little longer so a duplicate can show."""
    loop = asyncio.get_running_loop()
    end = loop.time() + timeout
    while len(pub.published) < n and loop.time() < end:
        await asyncio.sleep(0.01)
    await asyncio.sleep(0.15)
    return len(pub.published)


async def test_remote_emit_waits_for_the_callers_commit(stack, msg, reported):
    hold = Hold()
    stack.fake.scripts["tx-1"] = [PENDING, hold, COMMITTED]
    await _call(stack, msg)
    await asyncio.sleep(0.1)
    assert stack.pub.published == [], "the callee announced before its caller committed"
    hold.release()
    assert await _settle(stack.pub, 1) == 1, "announced other than once after the commit"
    assert reported == []


async def test_remote_emit_echoes_only_the_conn_id_on_the_wait(stack, msg):
    stack.fake.scripts["tx-1"] = [COMMITTED]
    await _call(stack, msg)
    assert len(stack.fake.calls) == 1, "no wait was registered with the coordinator"
    tx, md = stack.fake.calls[0]
    assert tx == "tx-1"
    assert md.get("w17-conn-id") == ("replica-2",), "the wait did not reach the transaction's replica"
    assert md == {"w17-conn-id": ("replica-2",)}, f"the wait forwarded the request's metadata: {sorted(md)}"


async def test_remote_emit_without_a_conn_id_sends_none(stack, msg):
    stack.fake.scripts["tx-1"] = [COMMITTED]
    await _call(stack, msg, conn="")
    assert len(stack.fake.calls) == 1, "no wait was registered with the coordinator"
    assert "w17-conn-id" not in stack.fake.calls[0][1]


async def test_remote_emit_dropped_on_rollback(stack, msg, reported):
    hold = Hold()
    stack.fake.scripts["tx-1"] = [PENDING, hold, NOT_COMMITTED]
    await _call(stack, msg)
    hold.release()
    assert await _settle(stack.pub, 1, timeout=0.3) == 0, "a rolled-back transaction's event was announced"
    assert reported == [], "an ordinary rollback is not reported"


async def test_remote_emit_published_once_when_a_busy_commit_is_retried(stack, msg, reported):
    # The first commit is refused (a call still holds the transaction): the
    # coordinator sends nothing for it. Only the retried commit answers.
    refused, retried = Hold(), Hold()
    stack.fake.scripts["tx-1"] = [PENDING, refused, retried, COMMITTED]
    await _call(stack, msg)
    refused.release()
    await asyncio.sleep(0.2)
    assert stack.pub.published == [], "announced while the refused commit left the transaction open"
    retried.release()
    assert await _settle(stack.pub, 1) == 1
    assert reported == []


async def test_remote_emit_of_an_already_committed_transaction_goes_out_now(stack, msg, reported):
    stack.fake.scripts["tx-1"] = [COMMITTED]
    await _call(stack, msg)
    assert len(stack.fake.calls) == 1, "no wait was registered with the coordinator"
    assert len(stack.pub.published) == 1, "an event of a committed transaction was not announced"
    assert reported == []


async def test_remote_emit_of_a_transaction_that_ended_without_committing_is_dropped(stack, msg, reported):
    stack.fake.scripts["tx-1"] = [NOT_COMMITTED]
    await _call(stack, msg)
    assert await _settle(stack.pub, 1, timeout=0.2) == 0
    assert reported == [
        'distx: dropped the deferred announcement of tx "tx-1": the transaction ended without committing '
        "before the emit registered"], reported


async def test_remote_emit_unknown_transaction_is_dropped_and_reported(stack, msg, reported):
    await _call(stack, msg, tx="tx-unknown")
    assert await _settle(stack.pub, 1, timeout=0.2) == 0, "announced for a transaction nobody can vouch for"
    assert len(reported) == 1 and reported[0].startswith(
        'distx: dropped the deferred announcement of tx "tx-unknown": its coordinator could not be asked how '
        "the transaction ends (") and "NOT_FOUND" in reported[0], reported


async def test_remote_emit_with_a_coordinator_without_the_outcome_service_goes_out_now(pool, msg, reported):
    s = await _stack(pool, serve_outcome=False)
    try:
        await _call(s, msg)
        assert len(s.pub.published) == 1, "an old coordinator must not cost every event"
        assert reported == [
            'distx: announcing inside transaction "tx-1" before it commits: its coordinator does not serve '
            "W17TransactionOutcome (upgrade the storage bundle), so the event cannot wait for the commit"], reported
    finally:
        await s.close()


async def test_remote_emit_dropped_when_the_coordinator_stops_answering(stack, msg, reported):
    hold = Hold()
    stack.fake.scripts["tx-1"] = [PENDING, hold, GRPCError(Status.UNAVAILABLE, "the replica went away")]
    await _call(stack, msg)
    hold.release()
    assert await _settle(stack.pub, 1, timeout=0.3) == 0
    assert len(reported) == 1 and reported[0].startswith(
        'distx: dropped the deferred announcement of tx "tx-1": the coordinator stopped answering before the '
        "transaction ended (") and reported[0].endswith(
        "), so whether it committed is unknown"), reported


async def test_remote_emit_dropped_when_the_wait_is_not_confirmed_in_time(stack, msg, reported, monkeypatch):
    monkeypatch.setattr(distx, "REGISTER_WAIT", 0.2, raising=False)
    stack.fake.scripts["tx-1"] = [Hold()]
    await _call(stack, msg)
    assert await _settle(stack.pub, 1, timeout=0.2) == 0
    assert reported == [
        'distx: dropped the deferred announcement of tx "tx-1": its coordinator could not be asked how the '
        "transaction ends (the coordinator did not confirm the wait within 0.2s)"], reported


async def test_remote_emit_wait_is_drained_like_other_after_commit_work(stack, msg):
    hold = Hold()
    stack.fake.scripts["tx-1"] = [PENDING, hold, COMMITTED]
    await _call(stack, msg)
    assert not await txscope.drain(0.1), "the shutdown drain does not see the wait for the commit"
    hold.release()
    assert await txscope.drain(3.0)
    assert len(stack.pub.published) == 1


async def test_remote_emit_no_coordinator_configured_publishes_now(pool, msg, reported):
    s = await _stack(pool, wire_outcome=False)
    try:
        await _call(s, msg)
        assert len(s.pub.published) == 1
        assert reported == []
    finally:
        await s.close()


async def test_remote_emit_in_process_scope_still_comes_first(stack, msg):
    # A method composed inside a transaction THIS process opened parks on the
    # in-process scope, whatever its incoming metadata says.
    ctx, scope = txscope.new(Context(incoming={"w17-tx-id": "tx-1"}))
    await stack.registry.self("NoteService").CreateNote(ctx, msg("CreateNoteRequest")(text="s"))
    assert stack.pub.published == [] and stack.fake.calls == []
    assert await scope.run_committed() == []
    assert len(stack.pub.published) == 1
