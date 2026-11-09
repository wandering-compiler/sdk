"""The served tier end to end, over real grpclib servers on loopback: a Python
stand-in for storage (served by the same Registry) and the business service."""

import asyncio
import base64

import pytest
from grpclib.client import Channel
from grpclib.const import Cardinality, Status
from grpclib.exceptions import GRPCError
from grpclib.server import Server

from w17_business import distx, events, for_user, paging, principal
from w17_business.client import Conn, ServiceClient, TxCoordinator
from w17_business.context import Context
from w17_business.errors import StatusDetailsCodec, error_detail
from w17_business.manifest import Emit, PayloadBinding
from w17_business.server import Registry, RegistrationError

EMIT = Emit(
    method="/conformance.v1.NoteService/CreateNote",
    channel="notes",
    topic="note.created",
    envelope="conformance.events.Event",
    envelope_field="v1_note_created",
    event="conformance.v1.NoteCreated",
    fields=(
        PayloadBinding(field="id", source="response", path=("id",)),
        PayloadBinding(field="text", source="request", path=("text",)),
        PayloadBinding(field="by", source="literal", value="py"),
    ),
)


class FakeStorage:
    """Storage stand-in: notes in memory, a coordinator that stages writes per
    transaction, and a record of what each call carried."""

    def __init__(self, pool):
        self.pool = pool
        self.M = lambda n: pool.message("conformance.v1." + n)
        self.rows = []
        self.staged = {}
        self.seen = []
        self.unavailable = 0
        self.n = 0

    # NoteStorage
    async def InsertNote(self, ctx, req):
        self.seen.append(("InsertNote", ctx))
        if self.unavailable:
            self.unavailable -= 1
            raise GRPCError(Status.UNAVAILABLE, "try again")
        self.n += 1
        note = self.M("Note")(id=self.n, text=req.text)
        tx = ctx.incoming.first("w17-tx-id")
        (self.staged.setdefault(tx, []) if tx else self.rows).append(note.text)
        return note

    async def ListNotes(self, ctx, req):
        self.seen.append(("ListNotes", ctx))
        limit = int(ctx.incoming.first(paging.LIMIT_MD_KEY) or 50)
        resp = self.M("ListNotesResponse")()
        for i, t in enumerate(self.rows[:limit]):
            resp.notes.add(id=i + 1, text=t)
        resp.paging.limit = limit
        # The page position, as a generated storage handler reports it.
        sink = paging.response_headers(ctx)
        if sink is not None:
            sink[paging.NEXT_MD_KEY] = ("next-of-page",)
            sink[paging.PREV_MD_KEY] = ("prev-of-page",)
            sink[paging.TOTAL_MD_KEY] = (str(len(self.rows)),)
        return resp

    async def Inspect(self, ctx, req):
        self.seen.append(("Inspect", ctx))
        sink = paging.response_headers(ctx)
        if sink is not None:
            # A sibling call's position, which must not reach the caller.
            sink[paging.NEXT_MD_KEY] = ("sibling",)
        resp = self.M("InspectResponse")()
        for k, v in ctx.incoming.items():
            resp.headers.add(key=k, values=list(v))
        rem = ctx.time_remaining()
        if rem is not None:
            resp.has_deadline = True
            resp.deadline_ms = int(rem * 1000)
        return resp


class FakeCoordinator:
    def __init__(self, storage):
        self.s = storage
        self.n = 0
        self.calls = []

    async def Begin(self, ctx, req):
        self.n += 1
        self.calls.append("begin")
        return distx._msg("BeginResponse")(tx_id=f"tx-{self.n}")

    async def Commit(self, ctx, req):
        self.calls.append("commit")
        self.s.rows.extend(self.s.staged.pop(req.tx_id, []))
        return distx._msg("CommitResponse")()

    async def Rollback(self, ctx, req):
        self.calls.append("rollback")
        self.s.staged.pop(req.tx_id, None)
        return distx._msg("RollbackResponse")()


class NoteService:
    def __init__(self, registry, storage: ServiceClient, tx):
        self.registry = registry
        self.storage = storage
        self.tx = tx
        self.M = lambda n: registry.pool.message("conformance.v1." + n)

    async def CreateNote(self, ctx, req):
        return await distx.run(ctx, self.tx, None,
                               lambda c: self.storage.InsertNote(c, self.M("InsertNoteRequest")(text=req.text)))

    async def CreateNotes(self, ctx, req):
        me = self.registry.self("conformance.v1.NoteService")
        out = self.M("CreateNotesResponse")()

        async def work(c):
            for t in req.texts:
                out.notes.append(await me.CreateNote(c, self.M("CreateNoteRequest")(text=t)))
            if req.fail:
                raise RuntimeError("refuse after the writes")

        await distx.run(ctx, self.tx, None, work)
        return out

    async def ListNotes(self, ctx, req):
        await self.storage.Inspect(ctx, self.M("InspectRequest")())
        return await self.storage.ListNotes(paging.forward(ctx), req)

    async def Inspect(self, ctx, req):
        return await self.storage.Inspect(ctx, req)

    async def Refuse(self, ctx, req):
        raise for_user(Status.FAILED_PRECONDITION, "NOTE_LOCKED", "This note is locked.", "text", dev="locked by export")

    async def Sleep(self, ctx, req):
        await asyncio.sleep(req.ms / 1000)
        return self.M("Note")(text="slept")

    async def TxErrors(self, ctx, req):
        raise KeyError("a bug")

    async def Whoami(self, ctx, req):
        return self.M("WhoamiResponse")(user_id=principal.user_id(ctx))

    # The companion's methods: the suite's NoteService has them; this file
    # tests the SDK without a companion (test_companion.py has one).
    async def Echo(self, ctx, req):
        raise GRPCError(Status.UNIMPLEMENTED, "no companion here")

    async def AttachFile(self, ctx, req):
        raise GRPCError(Status.UNIMPLEMENTED, "no companion here")


@pytest.fixture
async def stack(pool):
    storage = FakeStorage(pool)
    sreg = Registry(pool)
    sreg.register("conformance.v1.NoteStorage", storage)
    coord = FakeCoordinator(storage)
    sreg.register("w17.common.distx.W17DistributedTransaction", coord)
    ssrv = Server(sreg.served, status_details_codec=StatusDetailsCodec())
    await ssrv.start("127.0.0.1", 0)
    sport = ssrv._server.sockets[0].getsockname()[1]

    conn = Conn(f"127.0.0.1:{sport}", tls=False)
    pub = events.MemoryPublisher()
    breg = Registry(pool, (EMIT,), pub)
    svc = NoteService(breg, ServiceClient(conn, "conformance.v1.NoteStorage", pool), TxCoordinator(conn))
    breg.register("NoteService", svc)
    bsrv = Server(breg.served, status_details_codec=StatusDetailsCodec())
    await bsrv.start("127.0.0.1", 0)
    bport = bsrv._server.sockets[0].getsockname()[1]
    bconn = Conn(f"127.0.0.1:{bport}", tls=False)
    client = ServiceClient(bconn, "conformance.v1.NoteService", pool)

    class S:
        pass

    s = S()
    s.storage, s.coord, s.pub, s.client, s.registry, s.conn = storage, coord, pub, client, breg, conn
    s.bconn = bconn
    yield s
    bconn.close()
    conn.close()
    bsrv.close()
    ssrv.close()
    await bsrv.wait_closed()
    await ssrv.wait_closed()


def headers(resp):
    return {h.key: tuple(h.values) for h in resp.headers}


async def test_header_forwarding_over_the_wire(stack, msg):
    env = base64.b64encode(msg("AuthResp")(user_id="u-1").SerializeToString()).decode()
    ctx = Context(outgoing={
        "x-w17-user": env, "x-w17-scope-org_id": "o-1", "w17-label-tenant": "t", "x-w17-device": "d",
        "x-w17-data-scope": "org", "authorization": "Bearer nope", "x-w17-paging-limit": "2",
        "w17-language": "cs", "x-custom": "c",
    })
    resp = await stack.client.Inspect(ctx, msg("InspectRequest")())
    h = headers(resp)
    for k in ("x-w17-user", "x-w17-scope-org_id", "w17-label-tenant", "x-w17-device", "x-w17-data-scope"):
        assert k in h, k
    for k in ("authorization", "x-w17-paging-limit", "w17-language", "x-custom"):
        assert k not in h, k


async def test_deadline_propagates_and_expires(stack, msg):
    resp = await stack.client.Inspect(Context().with_timeout(5), msg("InspectRequest")())
    assert resp.has_deadline and 0 < resp.deadline_ms <= 5000
    with pytest.raises(GRPCError) as ei:
        await stack.client.Sleep(Context().with_timeout(0.2), msg("SleepRequest")(ms=3000))
    assert ei.value.status == Status.DEADLINE_EXCEEDED


async def test_error_detail_and_operator_prefix(stack, msg):
    with pytest.raises(GRPCError) as ei:
        await stack.client.Refuse(Context(), msg("RefuseRequest")())
    e = ei.value
    assert e.status == Status.FAILED_PRECONDITION
    assert e.message == "NoteService.Refuse: locked by export"
    d = error_detail(e)
    assert (d.code, d.message, d.field) == ("NOTE_LOCKED", "This note is locked.", "text")


async def test_an_unexpected_exception_is_internal_and_leaks_nothing(stack, msg):
    with pytest.raises(GRPCError) as ei:
        await stack.client.TxErrors(Context(), msg("TxErrorsRequest")())
    assert ei.value.status == Status.INTERNAL
    assert ei.value.message == "/conformance.v1.NoteService/TxErrors: internal error"


async def test_sdk_errors_answer_unknown(stack, msg):
    with pytest.raises(GRPCError) as ei:
        await stack.client.Whoami(Context(), msg("WhoamiRequest")())
    assert ei.value.status == Status.UNKNOWN
    assert "no authenticated caller" in ei.value.message


async def test_emit_after_success_and_through_self(stack, msg):
    note = await stack.client.CreateNote(Context(outgoing={"w17-label-tenant": "t"}), msg("CreateNoteRequest")(text="a"))
    assert stack.storage.rows == ["a"]
    assert len(stack.pub.published) == 1
    ch, topic, raw = stack.pub.published[0]
    assert (ch, topic) == ("notes", "note.created")
    env = stack.registry.pool.message("conformance.events.Event").FromString(raw)
    assert env.v1_note_created.id == note.id and env.v1_note_created.text == "a"
    assert dict(env.labels) == {"tenant": "t"}


async def test_nested_emits_wait_for_the_root_commit(stack, msg):
    await stack.client.CreateNotes(Context(), msg("CreateNotesRequest")(texts=["x", "y"]))
    assert stack.storage.rows == ["x", "y"]
    assert [stack.registry.pool.message("conformance.events.Event").FromString(r).v1_note_created.text
            for _, _, r in stack.pub.published] == ["x", "y"]
    assert stack.coord.calls == ["begin", "commit"], "one transaction: the nested runs joined"


async def test_rolled_back_transaction_drops_its_events(stack, msg):
    with pytest.raises(GRPCError) as ei:
        await stack.client.CreateNotes(Context(), msg("CreateNotesRequest")(texts=["x"], fail=True))
    assert ei.value.status == Status.INTERNAL
    assert stack.storage.rows == []
    assert stack.pub.published == []
    assert stack.coord.calls == ["begin", "rollback"]


async def test_paging_forward_reaches_the_one_call(stack, msg):
    stack.storage.rows = ["a", "b", "c"]
    resp = await stack.client.ListNotes(Context(outgoing={paging.LIMIT_MD_KEY: "2"}), msg("ListNotesRequest")())
    assert [n.text for n in resp.notes] == ["a", "b"]
    inspect_ctx = [c for name, c in stack.storage.seen if name == "Inspect"][-1]
    assert paging.LIMIT_MD_KEY not in inspect_ctx.incoming, "the sibling call got the page"


async def test_paging_forward_relays_the_page_position(stack, msg):
    # The business method answers with the position the FORWARDED storage call
    # reported (so the gateway can build the next cursor without knowing the
    # sort), not the one its sibling Inspect call reported, and nothing else.
    stack.storage.rows = ["a", "b", "c"]
    m = stack.client.method_info("ListNotes")
    _, header = await stack.bconn.unary(Context(outgoing={paging.LIMIT_MD_KEY: "2"}), m, msg("ListNotesRequest")())
    assert header.get_all(paging.NEXT_MD_KEY) == ("next-of-page",)
    assert header.get_all(paging.PREV_MD_KEY) == ("prev-of-page",)
    assert header.get_all(paging.TOTAL_MD_KEY) == ("3",)
    # A method that forwards nothing answers no position.
    _, header = await stack.bconn.unary(Context(), stack.client.method_info("Inspect"), msg("InspectRequest")())
    assert not header.get_all(paging.NEXT_MD_KEY)


async def test_client_retries_unavailable_three_times(stack, msg):
    storage = ServiceClient(stack.conn, "conformance.v1.NoteStorage", stack.registry.pool)
    stack.storage.unavailable = 2
    note = await storage.InsertNote(Context(), msg("InsertNoteRequest")(text="r"))
    assert note.text == "r"
    stack.storage.unavailable = 3
    with pytest.raises(GRPCError) as ei:
        await storage.InsertNote(Context(), msg("InsertNoteRequest")(text="r"))
    assert ei.value.status == Status.UNAVAILABLE
    assert stack.storage.unavailable == 0  # exactly three attempts


async def test_client_refuses_a_done_context(stack, msg):
    storage = ServiceClient(stack.conn, "conformance.v1.NoteStorage", stack.registry.pool)
    ctx = Context().with_cancel()
    ctx.cancel()
    before = len(stack.storage.seen)
    with pytest.raises(GRPCError) as ei:
        await storage.Inspect(ctx, msg("InspectRequest")())
    assert ei.value.status == Status.CANCELLED
    assert len(stack.storage.seen) == before


async def test_unreachable_storage_is_unavailable(pool, msg):
    conn = Conn("127.0.0.1:1", tls=False)
    try:
        with pytest.raises(GRPCError) as ei:
            await ServiceClient(conn, "conformance.v1.NoteStorage", pool).Inspect(Context(), msg("InspectRequest")())
        assert ei.value.status == Status.UNAVAILABLE
    finally:
        conn.close()


# --- registration ---------------------------------------------------------------


def test_missing_method_stops_registration(pool):
    class Partial:
        async def CreateNote(self, ctx, req):
            pass

    with pytest.raises(RegistrationError, match="missing method.*CreateNotes"):
        Registry(pool).register("conformance.v1.NoteService", Partial())


def test_snake_case_is_not_the_rpc_name(pool):
    class Snake(NoteService):
        pass

    Snake.create_note = Snake.CreateNote
    del_ok = Registry(pool)
    del_ok.register("conformance.v1.NoteService", Snake(del_ok, None, None))  # exact names present: fine

    class OnlySnake:
        async def create_note(self, ctx, req):
            pass

    with pytest.raises(RegistrationError, match="CreateNote"):
        Registry(pool).register("conformance.v1.NoteService", OnlySnake())


def test_sync_method_is_refused(pool):
    class Sync(NoteService):
        def CreateNote(self, ctx, req):  # not async
            return None

    reg = Registry(pool)
    with pytest.raises(RegistrationError, match="CreateNote.*async def"):
        reg.register("conformance.v1.NoteService", Sync(reg, None, None))


def test_unknown_or_duplicate_service(pool):
    reg = Registry(pool)
    with pytest.raises(RegistrationError, match="no service"):
        reg.register("nope.Service", object())
    reg.register("conformance.v1.NoteService", NoteService(reg, None, None))
    with pytest.raises(RegistrationError, match="already registered"):
        reg.register("conformance.v1.NoteService", NoteService(reg, None, None))
    with pytest.raises(RegistrationError, match="not registered"):
        reg.self("conformance.v1.NoteStorage")


async def test_direct_call_skips_the_emit_self_does_not(pool, msg):
    class Local(NoteService):
        async def CreateNote(self, ctx, req):
            return msg("Note")(id=1, text=req.text)

    pub = events.MemoryPublisher()
    reg = Registry(pool, (EMIT,), pub)
    impl = Local(reg, None, None)
    reg.register("conformance.v1.NoteService", impl)
    await impl.CreateNote(Context(), msg("CreateNoteRequest")(text="direct"))
    assert pub.published == []
    await reg.self("NoteService").CreateNote(Context(), msg("CreateNoteRequest")(text="served"))
    assert len(pub.published) == 1


async def test_draining_refuses_new_calls(stack, msg):
    stack.registry.draining = True
    with pytest.raises(GRPCError) as ei:
        await stack.client.Inspect(Context(), msg("InspectRequest")())
    assert ei.value.status == Status.UNAVAILABLE
    stack.registry.draining = False


async def test_server_streaming_is_served_and_cannot_emit(pool):
    # grpc.health.v1.Health has a server-streaming Watch; grpclib registers its
    # file in the default pool, which the SDK's pool is.
    import grpclib.health.v1.health_pb2  # noqa: F401

    class Health:
        async def Check(self, ctx, req):
            return pool.message("grpc.health.v1.HealthCheckResponse")(status=1)

        async def Watch(self, ctx, req):
            for s in (1, 2):
                yield pool.message("grpc.health.v1.HealthCheckResponse")(status=s)

    reg = Registry(pool)
    served = reg.register("grpc.health.v1.Health", Health())
    m = served.__mapping__()
    assert m["/grpc.health.v1.Health/Watch"].cardinality == Cardinality.UNARY_STREAM
    srv = Server(reg.served)
    await srv.start("127.0.0.1", 0)
    conn = Conn(f"127.0.0.1:{srv._server.sockets[0].getsockname()[1]}", tls=False)
    try:
        client = ServiceClient(conn, "grpc.health.v1.Health", pool)
        got = [r.status async for r in client.Watch(Context(), pool.message("grpc.health.v1.HealthCheckRequest")())]
        assert got == [1, 2]
    finally:
        conn.close()
        srv.close()
        await srv.wait_closed()

    bad = Emit(**{**EMIT.__dict__, "method": "/grpc.health.v1.Health/Watch"})
    with pytest.raises(RegistrationError, match="streaming"):
        Registry(pool, (bad,)).register("grpc.health.v1.Health", Health())

    class SyncGen:
        async def Check(self, ctx, req):
            pass

        async def Watch(self, ctx, req):  # a coroutine, not an async generator
            return None

    with pytest.raises(RegistrationError, match="Watch.*async generator"):
        Registry(pool).register("grpc.health.v1.Health", SyncGen())


async def test_channel_without_sdk_codec_still_reads_the_status(stack, msg):
    # A plain grpclib client (no details codec) sees the code and the message.
    ch = Channel("127.0.0.1", stack.client._conn.channel._port)
    try:
        from grpclib.client import UnaryUnaryMethod

        m = stack.client.method_info("Refuse")
        call = UnaryUnaryMethod(ch, m.path, m.request, m.response)
        with pytest.raises(GRPCError) as ei:
            await call(msg("RefuseRequest")())
        assert ei.value.status == Status.FAILED_PRECONDITION
    finally:
        ch.close()


# --- registry.self is Go's direct call: the callee gets the caller's ctx -------


def _envelope(msg, user_id):
    return base64.b64encode(msg("AuthResp")(user_id=user_id).SerializeToString()).decode()


async def test_self_hands_the_callee_the_callers_context_unchanged(pool, msg):
    # Go parity: s.self.Method(ctx, req) is a direct call. The callee sees the
    # caller's incoming metadata (w17-language and custom headers included),
    # whatever the caller put on its outgoing side, and the open transaction,
    # which its own distx.run JOINS.
    principal.set_auth_resp_type(msg("AuthResp"))
    try:
        seen = {}

        from w17_business.context import Metadata

        class Tx:
            def __init__(self):
                self.calls = []

            async def begin(self, ctx, req):
                self.calls.append("begin")
                return distx._msg("BeginResponse")(tx_id="tx-9"), Metadata()

        tx = Tx()

        class Local(NoteService):
            async def Inspect(self, ctx, req):
                seen["incoming"] = dict(ctx.incoming)
                seen["outgoing"] = dict(ctx.outgoing)
                seen["user"] = principal.user_id(ctx)
                h, joined = await distx.begin(ctx, tx)
                seen["joined"] = h.joined
                seen["tx"] = joined.outgoing.get_all(distx.TX_ID_HEADER)
                return msg("InspectResponse")()

        reg = Registry(pool)
        reg.register("conformance.v1.NoteService", Local(reg, None, None))
        incoming = {"x-w17-user": _envelope(msg, "person"), "w17-language": "cs", "x-custom": "c"}
        _, tx_ctx = await distx.begin(Context(incoming=incoming), tx)
        caller = tx_ctx.with_outgoing(tx_ctx.outgoing.with_set("x-mine", "m"))
        await reg.self("NoteService").Inspect(caller, msg("InspectRequest")())
        assert seen["incoming"] == dict(Metadata(incoming))
        assert seen["outgoing"] == {distx.TX_ID_HEADER: ("tx-9",), "x-mine": ("m",)}
        assert seen["user"] == "person"
        assert seen["joined"] is True and seen["tx"] == ("tx-9",)
        assert tx.calls == ["begin"], "the callee joined; it began nothing"
    finally:
        principal.set_auth_resp_type(None)


async def test_act_as_on_a_ctx_given_to_self_is_not_the_callees_principal(pool, msg):
    # As in Go: act_as sets the OUTGOING side (and strips the incoming one), and
    # the callee's readers read INCOMING — so a direct call does not see it.
    principal.set_auth_resp_type(msg("AuthResp"))
    try:
        seen = {}

        class Local(NoteService):
            async def Whoami(self, ctx, req):
                try:
                    seen["user"] = principal.user_id(ctx)
                except principal.NoPrincipalError:
                    seen["user"] = None
                return msg("WhoamiResponse")()

        reg = Registry(pool)
        reg.register("conformance.v1.NoteService", Local(reg, None, None))
        caller = Context(incoming={"x-w17-user": _envelope(msg, "person")})
        acted = principal.act_as(caller, msg("AuthResp")(user_id="robot"))
        await reg.self("NoteService").Whoami(acted, msg("WhoamiRequest")())
        assert seen["user"] is None
    finally:
        principal.set_auth_resp_type(None)


# --- an emit outlives the request that caused it ------------------------------


async def test_an_emit_outside_a_transaction_outlives_a_cancelled_caller(pool, msg):
    gate, started = asyncio.Event(), asyncio.Event()

    class SlowPub(events.MemoryPublisher):
        async def dispatch(self, ctx, channel, topic, envelope):
            started.set()
            await gate.wait()
            await super().dispatch(ctx, channel, topic, envelope)

    class Local(NoteService):
        async def CreateNote(self, ctx, req):
            return msg("Note")(id=1, text=req.text)

    pub = SlowPub()
    reg = Registry(pool, (EMIT,), pub)
    reg.register("conformance.v1.NoteService", Local(reg, None, None))
    task = asyncio.create_task(reg.self("NoteService").CreateNote(Context(), msg("CreateNoteRequest")(text="x")))
    await started.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    gate.set()
    for _ in range(100):
        if pub.published:
            break
        await asyncio.sleep(0.01)
    assert len(pub.published) == 1, "the committed write's event was lost with the request"


async def test_a_hanging_broker_is_bounded_by_the_publish_timeout(pool, msg, monkeypatch):
    monkeypatch.setattr(events, "DEFAULT_PUBLISH_TIMEOUT", 0.05)
    reported = []
    monkeypatch.setattr(events, "report_error", lambda ctx, e: reported.append(str(e)))

    class Hanging(events.MemoryPublisher):
        async def dispatch(self, ctx, channel, topic, envelope):
            await asyncio.Event().wait()

    class Local(NoteService):
        async def CreateNote(self, ctx, req):
            return msg("Note")(id=1, text=req.text)

    reg = Registry(pool, (EMIT,), Hanging())
    reg.register("conformance.v1.NoteService", Local(reg, None, None))
    note = await asyncio.wait_for(reg.self("NoteService").CreateNote(Context(), msg("CreateNoteRequest")(text="x")), 2)
    assert note.text == "x"
    assert reported and "note.created" in reported[0], reported
