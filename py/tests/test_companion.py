"""The companion's SDK half: manifest format 2, the companion connection
(no retries, no transaction or paging headers, the principal relayed), the
egress clients and the file stores — over a real grpclib server standing in
for the Go companion (the conformance suite runs the real one)."""

import base64
import copy
import hashlib
import json

import pytest
from google.protobuf import any_pb2
from google.rpc import error_details_pb2
from grpclib.const import Status
from grpclib.exceptions import GRPCError
from grpclib.server import Server

from w17_business import egress, files
from w17_business.client import CompanionConn, companion_pairs
from w17_business.context import Context
from w17_business.errors import ErrorInfo, StatusDetailsCodec, error_info
from w17_business.manifest import ManifestError, from_dict, load
from w17_business.runtime import BootError, Clients
from w17_business.server import Registry

from conftest import HERE, MANIFEST

SHOP = HERE / "testdata" / "shop.json"


def _conformance() -> dict:
    return json.loads(MANIFEST.read_text())


# --- manifest -----------------------------------------------------------------


def test_format_1_loads_as_before():
    m = load(SHOP)
    assert m.format == 1 and m.companion is None
    assert "companion" not in m.extra


def test_format_2_reads_the_companion():
    m = load(MANIFEST)
    c = m.companion
    assert m.format == 2 and c is not None
    assert (c.address_env, c.default_address) == ("NOTES_COMPANION_ADDR", "notes-business-companion:50051")
    assert [(e.client, e.service, e.dsn_env) for e in c.egress] == [
        ("echo", "clients.echo.EchoApi", "W17_CLIENT_ECHO_DSN")]
    assert c.files is not None and c.files.service == files.SERVICE
    assert [(x.name, x.writable) for x in c.files.connections] == [("notes-files", True), ("notes-media", False)]
    p = c.files.placements[0]
    assert (p.field, p.name, p.connection, p.bucket_path) == (
        "conformance.v1.NoteFile.file", "NoteFileFile", "notes-files", "/note-files")
    assert (p.expected_objects, p.max_per_bucket, p.max_size_bytes) == (100000, 100, 262144)


def test_a_64_bit_count_is_read_exactly():
    d = _conformance()
    d["companion"]["files"]["placements"][0]["expected_objects"] = 18446744073709551615
    assert from_dict(d).companion.files.placements[0].expected_objects == 18446744073709551615


def test_format_refusals():
    d = _conformance()
    d["format"] = 3
    with pytest.raises(ManifestError, match="reads formats 1 and 2"):
        from_dict(d)
    d["format"] = 1
    with pytest.raises(ManifestError, match="format 1 has no companion section"):
        from_dict(d)
    d["companion"] = None
    assert from_dict(d).companion is None
    d = _conformance()
    d["companion"]["files"]["placements"][0]["connection"] = "nope"
    with pytest.raises(ManifestError, match="not in companion.files.connections"):
        from_dict(d)
    d = _conformance()
    del d["companion"]["egress"][0]["dsn_env"]
    with pytest.raises(ManifestError, match="companion.egress\\[\\].dsn_env is required"):
        from_dict(d)


# --- ErrorInfo ------------------------------------------------------------------


def _info_any(domain="echo", reason="UPSTREAM_HTTP_404", **meta) -> any_pb2.Any:
    a = any_pb2.Any()
    a.Pack(error_details_pb2.ErrorInfo(domain=domain, reason=reason, metadata=meta))
    return a


def test_error_info_by_hand_and_decoded():
    packed = GRPCError(Status.NOT_FOUND, "echo: upstream answered HTTP 404",
                       [_info_any(http_status="404", attempts="1")])
    assert error_info(packed) == ErrorInfo("echo", "UPSTREAM_HTTP_404", {"http_status": "404", "attempts": "1"})
    decoded = GRPCError(Status.NOT_FOUND, "x", [error_details_pb2.ErrorInfo(domain="d", reason="R", metadata={"k": "v"})])
    assert egress.error_info(decoded) == ErrorInfo("d", "R", {"k": "v"})
    assert error_info(GRPCError(Status.NOT_FOUND, "no details")) is None
    assert error_info(ValueError("not a status")) is None
    assert error_info(None) is None
    broken = any_pb2.Any(type_url="type.googleapis.com/google.rpc.ErrorInfo", value=b"\x0a\xff")
    assert error_info(GRPCError(Status.INTERNAL, "x", [broken])) is None


def test_is_not_found_is_the_files_reason_only():
    nf = GRPCError(Status.NOT_FOUND, "x", [_info_any(files.DOMAIN, "FILE_NOT_FOUND", connection="c")])
    assert files.is_not_found(nf)
    assert not files.is_not_found(GRPCError(Status.NOT_FOUND, "x", [_info_any()]))  # an egress 404
    assert not files.is_not_found(GRPCError(Status.NOT_FOUND, "x"))
    assert not files.is_not_found(KeyError("x"))


def test_companion_pairs_strip_the_transaction_and_paging_headers():
    pairs = [("x-w17-user", "u"), ("w17-tx-id", "t"), ("W17-Conn-Id", "c"), ("x-w17-paging-limit", "2"),
             ("x-w17-paging-boundaries-bin", "b"), ("w17-language", "cs"), ("w17-label-tenant", "t")]
    assert companion_pairs(pairs) == [("x-w17-user", "u"), ("w17-language", "cs"), ("w17-label-tenant", "t")]


# --- over the wire ----------------------------------------------------------------


class FakeCompanion:
    """The Go companion's behaviour, as far as the SDK can see it."""

    def __init__(self, pool):
        self.pool = pool
        self.calls = []  # (method, incoming metadata, time remaining)
        self.objects: dict[str, bytes] = {}
        self.put_chunks: list[int] = []
        self.unavailable = 0
        self.n = 0

    def _seen(self, method, ctx):
        self.calls.append((method, dict(ctx.incoming.items()), ctx.time_remaining()))

    # clients.echo.EchoApi
    async def GetEcho(self, ctx, req):
        self._seen("GetEcho", ctx)
        if self.unavailable:
            self.unavailable -= 1
            raise GRPCError(Status.UNAVAILABLE, "echo: upstream answered HTTP 503",
                            [error_details_pb2.ErrorInfo(domain="echo", reason="UPSTREAM_HTTP_503")])
        if req.id == "missing":
            raise GRPCError(Status.NOT_FOUND, "echo: upstream answered HTTP 404",
                            [error_details_pb2.ErrorInfo(domain="echo", reason="UPSTREAM_HTTP_404",
                                                         metadata={"http_status": "404"})])
        return self.pool.message("clients.echo.Echo")(id=req.id, message="echo " + req.id)

    # w17.common.files.W17Files
    def _not_found(self, conn):
        return GRPCError(Status.NOT_FOUND, "w17.files: no stored file",
                         [error_details_pb2.ErrorInfo(domain=files.DOMAIN, reason="FILE_NOT_FOUND",
                                                      metadata={"connection": conn})])

    async def Put(self, ctx, reqs):
        self._seen("Put", ctx)
        field, body = None, bytearray()
        async for r in reqs:
            if r.WhichOneof("part") == "header":
                field = r.header.field
            else:
                self.put_chunks.append(len(r.chunk))
                body += r.chunk
        if len(body) > 262144:
            raise GRPCError(Status.INVALID_ARGUMENT, "w17.files: too large",
                            [error_details_pb2.ErrorInfo(domain=files.DOMAIN, reason="FILE_TOO_LARGE",
                                                         metadata={"field": field, "limit_bytes": "262144"})])
        self.n += 1
        handle = f"note-files/{hashlib.sha256(body).hexdigest()}-{self.n:032x}"
        self.objects[handle] = bytes(body)
        return self.pool.message("w17.common.files.PutResponse")(handle=handle)

    async def Open(self, ctx, req):
        self._seen("Open", ctx)
        body = self.objects.get(req.handle)
        if body is None:
            raise self._not_found(req.connection)
        for i in range(0, len(body), 64 << 10):
            yield self.pool.message("w17.common.files.OpenResponse")(chunk=body[i:i + (64 << 10)])

    async def Stat(self, ctx, req):
        self._seen("Stat", ctx)
        body = self.objects.get(req.handle)
        if body is None:
            raise self._not_found(req.connection)
        resp = self.pool.message("w17.common.files.StatResponse")(size=len(body))
        resp.mod_time.seconds, resp.mod_time.nanos = 1_700_000_000, 123_456_789
        return resp

    async def Delete(self, ctx, req):
        self._seen("Delete", ctx)
        self.objects.pop(req.handle, None)
        return self.pool.message("w17.common.files.DeleteResponse")()


@pytest.fixture
async def companion(pool):
    fake = FakeCompanion(pool)
    reg = Registry(pool)
    reg.register("clients.echo.EchoApi", fake)
    reg.register(files.SERVICE, fake)
    srv = Server(reg.served, status_details_codec=StatusDetailsCodec())
    await srv.start("127.0.0.1", 0)
    port = srv._server.sockets[0].getsockname()[1]
    clients = Clients(pool, load(MANIFEST), {
        "NOTES_COMPANION_ADDR": f"127.0.0.1:{port}",
        "NOTES_STORAGE_BACKEND_ADDR": "127.0.0.1:1",
    })
    fake.clients = clients
    yield fake
    clients.close()
    srv.close()
    await srv.wait_closed()


def _principal(pool) -> str:
    who = pool.message("conformance.v1.AuthResp")(user_id="u-1")
    return base64.b64encode(who.SerializeToString()).decode()


async def test_egress_call_relays_the_principal_and_nothing_transactional(companion, pool):
    ctx = Context(
        incoming={"x-w17-user": _principal(pool), "w17-label-tenant": "t-1"},
        outgoing={"w17-tx-id": "tx-9", "w17-conn-id": "c-9", "x-w17-paging-limit": "5", "w17-language": "cs"},
    ).with_timeout(5)
    echo = companion.clients.egress("echo")
    assert companion.clients.egress("clients.echo.EchoApi") is echo
    reply = await echo.GetEcho(ctx, pool.message("clients.echo.GetEchoRequest")(id="42"))
    assert reply.message == "echo 42"
    method, md, remaining = companion.calls[-1]
    assert method == "GetEcho"
    assert md["x-w17-user"] == (_principal(pool),) and md["w17-label-tenant"] == ("t-1",)
    assert md["w17-language"] == ("cs",)
    for k in ("w17-tx-id", "w17-conn-id", "x-w17-paging-limit"):
        assert k not in md, k
    assert remaining is not None and 0 < remaining <= 5


async def test_egress_errors_pass_through_with_their_error_info(companion, pool):
    with pytest.raises(GRPCError) as ei:
        await companion.clients.egress("echo").GetEcho(Context(), pool.message("clients.echo.GetEchoRequest")(id="missing"))
    assert ei.value.status == Status.NOT_FOUND
    assert egress.error_info(ei.value) == ErrorInfo("echo", "UPSTREAM_HTTP_404", {"http_status": "404"})


async def test_the_companion_connection_does_not_retry(companion, pool):
    companion.unavailable = 1
    with pytest.raises(GRPCError) as ei:
        await companion.clients.egress("echo").GetEcho(Context(), pool.message("clients.echo.GetEchoRequest")(id="1"))
    assert ei.value.status == Status.UNAVAILABLE
    assert [c[0] for c in companion.calls] == ["GetEcho"], "one attempt, no replay"


async def test_a_done_context_is_refused_before_sending(companion, pool):
    ctx = Context().with_timeout(0)
    with pytest.raises(GRPCError) as ei:
        await companion.clients.egress("echo").GetEcho(ctx, pool.message("clients.echo.GetEchoRequest")(id="1"))
    assert ei.value.status == Status.DEADLINE_EXCEEDED
    assert companion.calls == []


async def test_unknown_names_are_refused_naming_the_known_ones(companion):
    with pytest.raises(KeyError, match="'echo'"):
        companion.clients.egress("nope")
    with pytest.raises(KeyError, match="'notes-files', 'notes-media'"):
        companion.clients.store("nope")


async def test_file_round_trip(companion):
    store = companion.clients.store("notes-files")
    assert store.name == "notes-files" and store.writable
    body = bytes(i % 251 for i in range(200 << 10))
    ctx = Context(outgoing={"w17-tx-id": "tx-1"})
    handle = await store.put(ctx, "conformance.v1.NoteFile.file", body)
    assert companion.put_chunks == [64 << 10, 64 << 10, 64 << 10, 8 << 10]
    assert handle.startswith("note-files/" + hashlib.sha256(body).hexdigest())
    info = await store.stat(ctx, handle)
    assert info.size == len(body)
    assert info.mod_time.timestamp() == pytest.approx(1_700_000_000.123456, abs=1e-6)
    assert await store.read(ctx, handle) == body
    chunks = [c async for c in store.open(ctx, handle)]
    assert len(chunks) == 4 and b"".join(chunks) == body
    await store.delete(ctx, handle)
    with pytest.raises(GRPCError) as ei:
        await store.read(ctx, handle)
    assert files.is_not_found(ei.value)
    with pytest.raises(GRPCError) as ei:
        await store.stat(ctx, handle)
    assert files.is_not_found(ei.value)
    assert all("w17-tx-id" not in md for _, md, _ in companion.calls)


async def test_put_bodies_are_rechunked(companion):
    store = companion.clients.store("notes-files")

    def parts():
        yield b"a" * 100_000
        yield bytearray(b"b" * 30_000)
        yield memoryview(b"c" * 10)

    async def aparts():
        for p in parts():
            yield p

    for body in (parts(), aparts()):
        companion.put_chunks.clear()
        handle = await store.put(Context(), "NoteFile.file", body)
        assert companion.put_chunks == [64 << 10, 130_010 - (64 << 10)]
        assert await store.read(Context(), handle) == b"a" * 100_000 + b"b" * 30_000 + b"c" * 10
    companion.put_chunks.clear()
    handle = await store.put(Context(), "NoteFile.file", b"")
    assert companion.put_chunks == [] and await store.read(Context(), handle) == b""


async def test_put_refusals(companion):
    store = companion.clients.store("notes-files")
    with pytest.raises(GRPCError) as ei:
        await store.put(Context(), "NoteFile.file", b"x" * (262144 + 1))
    assert ei.value.status == Status.INVALID_ARGUMENT
    assert error_info(ei.value).reason == "FILE_TOO_LARGE"
    with pytest.raises(KeyError, match="not a field this tier writes files for"):
        await store.put(Context(), "Nope.file", b"x")
    media = companion.clients.store("notes-media")
    assert not media.writable
    with pytest.raises(KeyError, match="stored on notes-files, not on notes-media"):
        await media.put(Context(), "NoteFile.file", b"x")
    n = len(companion.calls)
    with pytest.raises(PermissionError, match="notes-media"):
        await media.delete(Context(), "any")
    assert len(companion.calls) == n, "refused before any call"
    with pytest.raises(TypeError):
        await store.put(Context(), "NoteFile.file", "text, not bytes")

    def broken():
        yield b"x"
        raise OSError("the author's file is gone")

    with pytest.raises(OSError, match="the author's file is gone"):
        await store.put(Context(), "NoteFile.file", broken())


def test_resolve_field_short_form_must_be_unambiguous():
    m = load(MANIFEST)
    ps = list(m.companion.files.placements)
    other = copy.replace(ps[0], field="other.v1.NoteFile.file")
    assert files.resolve_field("NoteFile.file", ps).field == "conformance.v1.NoteFile.file"
    with pytest.raises(KeyError, match="ambiguous"):
        files.resolve_field("NoteFile.file", [*ps, other])
    with pytest.raises(KeyError):
        files.resolve_field("file", ps)


async def test_boot_refusals(pool):
    d = _conformance()
    d["companion"]["egress"][0]["service"] = "clients.nope.NopeApi"
    with pytest.raises(BootError, match="clients.nope.NopeApi"):
        Clients(pool, from_dict(d), {"NOTES_COMPANION_ADDR": "127.0.0.1:1"})
    d = _conformance()
    d["companion"]["default_address"] = ""
    with pytest.raises(BootError, match="NOTES_COMPANION_ADDR is not set"):
        Clients(pool, from_dict(d), {})
    with pytest.raises(BootError, match="has no port"):
        Clients(pool, load(MANIFEST), {"NOTES_COMPANION_ADDR": "nowhere"})
    # The default address is used when the variable is unset.
    c = Clients(pool, load(MANIFEST), {})
    assert c._companion.addr == "notes-business-companion:50051"
    assert isinstance(c._companion, CompanionConn) and c._companion is not c._conn
    c.close()
