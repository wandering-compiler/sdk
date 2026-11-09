import asyncio

import pytest
from grpclib.const import Status
from grpclib.exceptions import GRPCError

from w17_business import distx, txscope
from w17_business.context import Context, Metadata


class ScriptedTx:
    """A coordinator whose calls can each be made to fail, recording their order
    and the routing headers each carried. Like a real gRPC client it REFUSES a
    call on a done context — the property the detached rollback exists for."""

    def __init__(self, conn_id="", commit_err=None, rollback_err=None, begin_err=None):
        self.calls = []
        self.headers = []
        self.conn_id = conn_id
        self.commit_err = commit_err
        self.rollback_err = rollback_err
        self.begin_err = begin_err
        self.n = 0

    def _check(self, ctx):
        if ctx.done():
            raise GRPCError(Status.CANCELLED, ctx.err())

    async def begin(self, ctx, req):
        self._check(ctx)
        self.calls.append("begin")
        if self.begin_err:
            raise self.begin_err
        self.n += 1
        header = Metadata({distx.CONN_ID_HEADER: self.conn_id} if self.conn_id else None)
        return distx._msg("BeginResponse")(tx_id=f"tx-{self.n}"), header

    async def commit(self, ctx, req):
        self._check(ctx)
        self.calls.append("commit")
        self.headers.append(ctx.outgoing)
        if self.commit_err:
            raise self.commit_err
        return distx._msg("CommitResponse")()

    async def rollback(self, ctx, req):
        self._check(ctx)
        self.calls.append("rollback")
        self.headers.append(ctx.outgoing)
        if self.rollback_err:
            raise self.rollback_err
        return distx._msg("RollbackResponse")()

    def seq(self):
        return ",".join(self.calls)


async def test_run_commits_and_returns_the_result():
    tx = ScriptedTx()
    seen = {}

    async def fn(ctx):
        seen["tx"] = ctx.outgoing.get_all(distx.TX_ID_HEADER)
        return 42

    assert await distx.run(Context(), tx, distx.begin_request("main"), fn) == 42
    assert tx.seq() == "begin,commit"
    assert seen["tx"] == ("tx-1",)


async def test_run_rolls_back_and_reraises():
    tx = ScriptedTx()

    async def fn(ctx):
        raise ValueError("boom")

    with pytest.raises(ValueError, match="boom"):
        await distx.run(Context(), tx, None, fn)
    assert tx.seq() == "begin,rollback"


async def test_run_names_a_failed_rollback_on_the_functions_error():
    tx = ScriptedTx(rollback_err=GRPCError(Status.INTERNAL, "db gone"))

    async def fn(ctx):
        raise ValueError("boom")

    with pytest.raises(ValueError) as ei:
        await distx.run(Context(), tx, None, fn)
    assert any("rollback also failed" in n for n in ei.value.__notes__)


async def test_rollback_is_detached_from_a_cancelled_caller():
    tx = ScriptedTx()
    ctx = Context().with_cancel()

    async def fn(c):
        ctx.cancel()  # the client hung up mid-transaction
        raise asyncio.CancelledError()

    with pytest.raises(asyncio.CancelledError):
        await distx.run(ctx, tx, None, fn)
    assert tx.seq() == "begin,rollback", "the rollback must reach the server although the caller is gone"


async def test_a_done_context_refuses_to_commit():
    tx = ScriptedTx()
    ctx = Context().with_cancel()

    async def fn(c):
        ctx.cancel()
        return None

    with pytest.raises(distx.DistxError, match="not committing"):
        await distx.run(ctx, tx, None, fn)
    assert tx.seq() == "begin,rollback"


async def test_commit_failure_is_raised_and_not_rolled_back():
    tx = ScriptedTx(commit_err=GRPCError(Status.ABORTED, "serialization failure"))

    async def fn(c):
        return 1

    with pytest.raises(GRPCError) as ei:
        await distx.run(Context(), tx, None, fn)
    assert ei.value.status == Status.ABORTED  # surfaced: the caller retries the unit of work
    assert tx.seq() == "begin,commit"
    assert "distx.run: commit" in ei.value.__notes__


async def test_begin_failure_does_not_call_fn():
    tx = ScriptedTx(begin_err=GRPCError(Status.UNAVAILABLE, "down"))
    called = False

    async def fn(c):
        nonlocal called
        called = True

    with pytest.raises(GRPCError):
        await distx.run(Context(), tx, None, fn)
    assert not called


async def test_conn_id_is_echoed_and_headers_are_set_not_appended():
    tx = ScriptedTx(conn_id="replica-7")
    stale = Context(outgoing=[(distx.TX_ID_HEADER, "old"), (distx.CONN_ID_HEADER, "old-conn")])
    h, ctx = await distx.begin(stale, tx, distx.begin_request())
    assert ctx.outgoing.get_all(distx.TX_ID_HEADER) == ("tx-1",)
    assert ctx.outgoing.get_all(distx.CONN_ID_HEADER) == ("replica-7",)
    assert h.conn_id == "replica-7" and h.tx_id == "tx-1"
    await h.commit(Context())  # a fresh context: the handle re-attaches its routing
    assert tx.headers[-1].get_all(distx.CONN_ID_HEADER) == ("replica-7",)
    assert tx.headers[-1].get_all(distx.TX_ID_HEADER) == ("tx-1",)


async def test_no_conn_id_removes_a_stale_one():
    tx = ScriptedTx()
    _, ctx = await distx.begin(Context(outgoing={distx.CONN_ID_HEADER: "stale"}), tx)
    assert distx.CONN_ID_HEADER not in ctx.outgoing


async def test_nested_run_joins_one_transaction():
    tx = ScriptedTx()
    ids = []

    async def inner(c):
        ids.append(c.outgoing.get_all(distx.TX_ID_HEADER))

    async def outer(c):
        ids.append(c.outgoing.get_all(distx.TX_ID_HEADER))
        await distx.run(c, tx, distx.begin_request(), inner)

    await distx.run(Context(), tx, distx.begin_request(), outer)
    assert tx.seq() == "begin,commit"
    assert ids == [("tx-1",), ("tx-1",)]


async def test_joined_rollback_makes_the_root_rollback_only():
    tx = ScriptedTx()

    async def inner(c):
        raise ValueError("inner refuses")

    async def outer(c):
        with pytest.raises(ValueError):
            await distx.run(c, tx, distx.begin_request(), inner)
        # swallowed: the caller carries on as if nothing happened

    with pytest.raises(distx.RollbackOnlyError):
        await distx.run(Context(), tx, distx.begin_request(), outer)
    assert tx.seq() == "begin,rollback"


@pytest.mark.parametrize("req", [
    distx.begin_request(connection_name="audit"),
    distx.begin_request(isolation=distx.Isolation.SERIALIZABLE),
])
async def test_nested_mismatch(req):
    tx = ScriptedTx()

    async def outer(c):
        await distx.begin(c, tx, req)

    with pytest.raises(distx.NestedMismatchError):
        await distx.run(Context(), tx, distx.begin_request(connection_name="main", isolation=distx.Isolation.READ_COMMITTED)
                        if req.isolation else distx.begin_request(connection_name="main"), outer)


async def test_nested_same_isolation_or_unspecified_joins():
    tx = ScriptedTx()

    async def outer(c):
        h, _ = await distx.begin(c, tx, distx.begin_request(isolation=distx.Isolation.SERIALIZABLE))
        assert h.joined
        h2, _ = await distx.begin(c, tx, distx.begin_request())
        assert h2.joined

    await distx.run(Context(), tx, distx.begin_request(isolation=distx.Isolation.SERIALIZABLE), outer)
    assert tx.seq() == "begin,commit"


async def test_handle_finished():
    tx = ScriptedTx()
    h, _ = await distx.begin(Context(), tx)
    await h.commit(Context())
    with pytest.raises(distx.HandleFinishedError, match="already committed"):
        await h.commit(Context())
    with pytest.raises(distx.HandleFinishedError):
        await h.rollback(Context())
    assert tx.seq() == "begin,commit"


async def test_a_failed_attempt_does_not_use_the_handle_up():
    tx = ScriptedTx(commit_err=GRPCError(Status.FAILED_PRECONDITION, "no"))
    h, _ = await distx.begin(Context(), tx)
    with pytest.raises(GRPCError):
        await h.commit(Context())
    tx.commit_err = None
    # The coordinator answered (FAILED_PRECONDITION means "did not commit"):
    # the handle is usable again.
    await h.rollback(Context())


async def test_commit_lost_in_transport_then_rolled_back_is_an_ordinary_failure():
    tx = ScriptedTx(commit_err=GRPCError(Status.UNAVAILABLE, "reset"))
    ran = []
    h, ctx = await distx.begin(Context(), tx)
    await distx.on_commit(ctx, lambda: ran.append(1))
    with pytest.raises(GRPCError) as ei:
        await h.commit(Context())
    assert ei.value.status == Status.UNAVAILABLE
    assert any("did not take effect" in n for n in ei.value.__notes__)
    assert tx.seq() == "begin,commit,rollback"
    assert ran == []


async def test_commit_outcome_unknown():
    tx = ScriptedTx(commit_err=GRPCError(Status.DEADLINE_EXCEEDED, "lost"),
                    rollback_err=GRPCError(Status.NOT_FOUND, "no such tx"))
    ran = []
    h, ctx = await distx.begin(Context(), tx)
    await distx.on_commit(ctx, lambda: ran.append(1))
    with pytest.raises(distx.CommitOutcomeUnknownError):
        await h.commit(Context())
    assert ran == [], "work parked on a commit of unknown outcome must not run"
    assert tx.seq() == "begin,commit,rollback"


async def test_on_commit_order_and_discard():
    tx = ScriptedTx()
    seen = []

    async def fn(c):
        await distx.on_commit(c, lambda: seen.append("a"))

        async def b():
            seen.append("b")

        assert await distx.on_commit(c, b) is txscope.Outcome.DEFERRED
        assert seen == []

    await distx.run(Context(), tx, None, fn)
    assert seen == ["a", "b"]

    seen.clear()

    async def failing(c):
        await distx.on_commit(c, lambda: seen.append("x"))
        raise RuntimeError("no")

    with pytest.raises(RuntimeError):
        await distx.run(Context(), tx, None, failing)
    assert seen == []


async def test_on_commit_outside_a_transaction_runs_now():
    seen = []
    assert await distx.on_commit(Context(), lambda: seen.append(1)) is txscope.Outcome.NO_SCOPE
    assert seen == [1]


async def test_on_commit_failure_after_commit_does_not_fail_the_commit():
    tx = ScriptedTx()
    seen = []

    async def fn(c):
        await distx.on_commit(c, lambda: 1 / 0)
        await distx.on_commit(c, lambda: seen.append("still runs"))

    await distx.run(Context(), tx, None, fn)
    assert seen == ["still runs"]


async def test_on_commit_in_a_joined_run_waits_for_the_outer_commit():
    for outer_fails in (False, True):
        tx = ScriptedTx()
        seen = []

        async def inner(c):
            await distx.on_commit(c, lambda: seen.append("inner"))

        async def outer(c):
            await distx.run(c, tx, None, inner)
            assert seen == []
            if outer_fails:
                raise RuntimeError("outer refuses")

        try:
            await distx.run(Context(), tx, None, outer)
        except RuntimeError:
            pass
        assert seen == ([] if outer_fails else ["inner"])


async def test_after_the_transaction_ended():
    tx = ScriptedTx()
    kept = {}

    async def fn(c):
        kept["ctx"] = c

    await distx.run(Context(), tx, None, fn)
    seen = []
    assert await distx.on_commit(kept["ctx"], lambda: seen.append(1)) is txscope.Outcome.ALREADY_COMMITTED
    assert seen == [1]
    # And a begin on a context whose transaction ended opens a new one.
    h, _ = await distx.begin(kept["ctx"], tx)
    assert not h.joined


async def test_run_refuses_nil_arguments():
    with pytest.raises(ValueError):
        await distx.run(Context(), None, None, lambda c: None)
    with pytest.raises(ValueError):
        await distx.run(Context(), ScriptedTx(), None, None)


async def test_sync_fn_is_accepted():
    tx = ScriptedTx()
    assert await distx.run(Context(), tx, None, lambda c: "sync") == "sync"


# --- after the commit: the request may be gone ------------------------------


async def _settled(cond, timeout=2.0):
    loop = asyncio.get_running_loop()
    end = loop.time() + timeout
    while not cond() and loop.time() < end:
        await asyncio.sleep(0.01)
    return cond()


async def test_a_cancelled_caller_does_not_cut_off_the_parked_work():
    # The client hangs up while the first hook runs: storage has committed, so
    # every hook (and the events among them) must still run to its end.
    tx = ScriptedTx()
    gate, started, ran = asyncio.Event(), asyncio.Event(), []

    async def slow():
        started.set()
        await gate.wait()
        ran.append("slow")

    async def fn(ctx):
        await distx.on_commit(ctx, slow)
        await distx.on_commit(ctx, lambda: ran.append("next"))

    task = asyncio.create_task(distx.run(Context(), tx, None, fn))
    await started.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    gate.set()
    assert await _settled(lambda: ran == ["slow", "next"]), ran
    assert await txscope.drain(1.0)


async def test_one_hook_cannot_stop_the_rest(monkeypatch):
    reported = []
    monkeypatch.setattr(distx, "report_error", lambda ctx, e: reported.append(str(e)))

    class Stop(BaseException):
        pass

    tx = ScriptedTx()
    ran = []

    async def cancelled():
        raise asyncio.CancelledError()

    def stop():
        raise Stop("a BaseException")

    async def fn(ctx):
        await distx.on_commit(ctx, cancelled)
        await distx.on_commit(ctx, stop)
        await distx.on_commit(ctx, lambda: ran.append("last"))

    await distx.run(Context(), tx, None, fn)
    assert ran == ["last"]
    assert len(reported) == 2 and all("parked on committed transaction tx-1" in r for r in reported), reported


async def test_drain_waits_for_work_that_outlived_its_request():
    gate, done = asyncio.Event(), []

    async def work():
        await gate.wait()
        done.append(1)

    caller = asyncio.create_task(txscope.shielded(work()))
    await asyncio.sleep(0)
    caller.cancel()
    with pytest.raises(asyncio.CancelledError):
        await caller
    assert not await txscope.drain(0.05), "the work is still running"
    gate.set()
    assert await txscope.drain(1.0) and done == [1]


async def test_cancel_mid_commit_with_unknown_outcome_uses_the_handle_up():
    # Cancelled while the commit is in flight, and the settling rollback fails
    # too: the commit may have landed, so the handle must refuse another one.
    tx = ScriptedTx(rollback_err=GRPCError(Status.NOT_FOUND, "no such tx"))
    h, ctx = await distx.begin(Context(), tx)
    gate = asyncio.Event()
    commit = tx.commit

    async def hanging_commit(c, req):
        await commit(c, req)
        await gate.wait()

    tx.commit = hanging_commit
    task = asyncio.create_task(h.commit(Context()))
    await asyncio.sleep(0.01)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    tx.commit = commit
    with pytest.raises(distx.HandleFinishedError, match="outcome unknown"):
        await h.commit(Context())
    assert tx.seq() == "begin,commit,rollback"


async def test_drain_waits_for_work_that_starts_while_it_waits():
    # Work started while the drain already waits (a hook of the first piece
    # starting the next, a handler cancelled late) must be awaited too: one
    # snapshot taken at the start would miss it.
    done = []

    async def second():
        await asyncio.sleep(0.1)
        done.append("second")

    async def first():
        await asyncio.sleep(0.05)
        asyncio.ensure_future(txscope.shielded(second()))
        done.append("first")

    asyncio.ensure_future(txscope.shielded(first()))
    await asyncio.sleep(0)
    assert await txscope.drain(2.0)
    assert done == ["first", "second"], "the drain returned before the work started while it waited"


def test_a_cancelled_task_stops_running_hooks_so_asyncio_run_returns():
    # asyncio.run cancels the leftover tasks at teardown and waits for them: a
    # run_committed that swallowed its OWN cancellation would move on to the
    # next stuck hook and hang the exit forever.
    import threading

    async def stuck():
        await asyncio.Event().wait()

    async def main():
        _, scope = txscope.new(Context())
        scope._park(stuck)
        scope._park(stuck)
        asyncio.ensure_future(scope.run_committed())
        await asyncio.sleep(0.01)

    t = threading.Thread(target=asyncio.run, args=(main(),), daemon=True)
    t.start()
    t.join(5)
    assert not t.is_alive(), "asyncio.run never returned: a cancelled task kept running stuck hooks"


async def test_a_hooks_own_cancellation_is_still_caught_per_hook():
    _, scope = txscope.new(Context())
    ran = []

    async def own_cancel():
        raise asyncio.CancelledError()

    scope._park(own_cancel)
    scope._park(lambda: ran.append(1))
    failures = await scope.run_committed()
    assert ran == [1] and len(failures) == 1


async def test_a_second_cancel_during_the_settle_still_uses_the_handle_up():
    # Cancelled mid-commit, then cancelled AGAIN while the settling rollback is
    # in flight: the handle must already refuse another commit.
    tx = ScriptedTx()
    h, ctx = await distx.begin(Context(), tx)
    commit_gate, rollback_gate = asyncio.Event(), asyncio.Event()
    commit, rollback = tx.commit, tx.rollback

    async def hanging_commit(c, req):
        await commit(c, req)
        await commit_gate.wait()

    async def hanging_rollback(c, req):
        await rollback_gate.wait()
        return await rollback(c, req)

    tx.commit, tx.rollback = hanging_commit, hanging_rollback
    task = asyncio.create_task(h.commit(Context()))
    await asyncio.sleep(0.01)
    task.cancel()
    await asyncio.sleep(0.01)  # now inside the settle
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    tx.commit = commit
    with pytest.raises(distx.HandleFinishedError):
        await h.commit(Context())
    rollback_gate.set()
    assert await txscope.drain(1.0)
    assert tx.seq() == "begin,commit,rollback"
