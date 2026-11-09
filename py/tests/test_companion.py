"""The companion's SDK half: manifest format 2, the companion connection
(no retries, no transaction or paging headers, the principal relayed), the
egress clients and the file stores — over a real grpclib server standing in
for the Go companion (the conformance suite runs the real one)."""

import asyncio
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
from w17_business.manifest import FilePlacement, ManifestError, from_dict, load
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


def test_companion_refusals_match_the_go_companion():
    # The set bizcompanion.Companion.check refuses, refused here too.
    def refused(edit, match):
        d = _conformance()
        edit(d["companion"])
        with pytest.raises(ManifestError, match=match):
            from_dict(d)

    refused(lambda c: c.clear(), "companion.address_env is required")
    refused(lambda c: c.update(address_env=""), "companion.address_env is required")
    refused(lambda c: c["files"]["placements"].append(dict(c["files"]["placements"][0])),
            "companion.files.placements: conformance.v1.NoteFile.file is listed twice")
    refused(lambda c: c["egress"].append(dict(c["egress"][0])), "companion.egress: client echo is listed twice")
    refused(lambda c: c["egress"].append({**c["egress"][0], "client": "echo2", "dsn_env": "W17_CLIENT_ECHO2_DSN"}),
            "companion.egress: service clients.echo.EchoApi is listed twice")
    for bad in ("false", "true", 0, 1, []):
        refused(lambda c, bad=bad: c["files"]["connections"][0].update(writable=bad),
                "companion.files.connections\\[\\].writable must be true or false")
    # Absent or null is false; a JSON bool is read as it is.
    d = _conformance()
    d["companion"]["files"]["connections"][0]["writable"] = None
    d["companion"]["files"]["connections"][1].pop("writable", None)
    assert [c.writable for c in from_dict(d).companion.files.connections] == [False, False]

    refused(lambda c: c["files"].update(service="w17.common.files.W17Files "),
            "the companion serves w17.common.files.W17Files")
    refused(lambda c: c.update(ADDRESS_ENV=c["address_env"]), "key 'ADDRESS_ENV' is not 'address_env'")
    # Go's encoding/json folds U+017F (long s) to s and U+212A (Kelvin) to k.
    refused(lambda c: c["egress"][0].update({"ſervice": "x"}), "key 'ſervice' is not 'service'")
    refused(lambda c: c["files"]["placements"][0].update({"bucKet_path": "/x"}),
            "key 'bucKet_path' is not 'bucket_path'")


# Every field refuses a value of another JSON type, and an integer one anything
# but an integer in its Go type's range — the set the Go companion's
# TestLoadRefusals carries (bizcompanion decodes into typed structs).
_STRINGS = {
    (): ("address_env", "default_address"),
    ("egress", 0): ("client", "service", "dsn_env"),
    ("files",): ("service",),
    ("files", "connections", 0): ("name", "root_env", "default_root"),
    ("files", "placements", 0): ("field", "name", "connection", "bucket_path"),
}
_BAD = [
    (where, f, bad, "must be a string")
    for where, fs in _STRINGS.items() for f in fs for bad in (5, True, [], {})
] + [
    (("files", "connections", 0), "writable", bad, "must be true or false") for bad in ("false", 0, [])
] + [
    (("files", "placements", 0), f, bad, "must be an integer from 0 to 18446744073709551615")
    for f in ("expected_objects", "max_size_bytes") for bad in (-1, "100", 1.5, 1.0, True, 2**64)
] + [
    (("files", "placements", 0), "max_per_bucket", bad, "must be an integer from 0 to 4294967295")
    for bad in (-1, 2**32, 2**33, "1", 1.0)
] + [
    ((), "egress", {}, "companion.egress must be a list"),
    ((), "egress", ["echo"], "companion.egress\\[\\] must be an object"),
    ((), "egress", [None], "companion.egress\\[\\].client is required"),
    ((), "files", [], "companion.files must be an object"),
    (("files",), "connections", {}, "companion.files.connections must be a list"),
    (("files",), "connections", [None], "companion.files.connections\\[\\].name is required"),
    (("files",), "placements", "x", "companion.files.placements must be a list"),
    (("files",), "placements", [None], "companion.files.placements\\[\\].field is required"),
]


@pytest.mark.parametrize(("where", "key", "bad", "match"), _BAD)
def test_companion_types_match_the_go_companion(where, key, bad, match):
    d = _conformance()
    at = d["companion"]
    for step in where:
        at = at[step]
    at[key] = bad
    with pytest.raises(ManifestError, match=match):
        from_dict(d)


def test_the_companion_section_must_be_an_object():
    d = _conformance()
    d["companion"] = "egress"
    with pytest.raises(ManifestError, match="companion must be an object"):
        from_dict(d)


@pytest.mark.parametrize("literal", ["-0", "1.0", "1e2", "100.00", "18446744073709551616", "-1"])
def test_an_integer_field_reads_only_an_integer_literal(tmp_path, literal):
    # As Go's encoding/json: -0, 1.0 and 1e2 are not unsigned integers.
    src = MANIFEST.read_text().replace('"max_size_bytes": 262144', f'"max_size_bytes": {literal}')
    assert literal in src
    path = tmp_path / "w17-business.json"
    path.write_text(src)
    with pytest.raises(ManifestError, match="max_size_bytes must be an integer"):
        load(path)


def test_the_companion_section_reads_as_the_go_companion(tmp_path):
    # A key's last occurrence wins (the Go companion reads it so too), null is
    # a field's zero value, an unknown key is ignored.
    src = MANIFEST.read_text()
    head, sep, tail = src.partition('"companion": {')
    assert sep
    path = tmp_path / "w17-business.json"
    path.write_text(head + sep + '"address_env": 5, "egress": {}, "files": {"service": 1},' + tail)
    c = load(path).companion
    assert c.address_env == "NOTES_COMPANION_ADDR" and len(c.egress) == 1 and c.files.service == files.SERVICE
    d = _conformance()
    d["companion"]["default_address"] = None
    d["companion"]["Unknown"] = 5
    conn = d["companion"]["files"]["connections"][0]
    conn["default_root"] = conn["writable"] = None
    p = d["companion"]["files"]["placements"][0]
    for k in ("name", "bucket_path", "expected_objects", "max_per_bucket", "max_size_bytes"):
        p[k] = None
    c = from_dict(d).companion
    assert c.default_address == "" and c.files.connections[0].default_root == ""
    assert c.files.placements[0] == FilePlacement(field="conformance.v1.NoteFile.file", name="", connection="notes-files")
    d["companion"]["egress"] = d["companion"]["files"] = None
    c = from_dict(d).companion
    assert c.egress == () and c.files is None


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
        self.put_ends: list[int] = []  # the size of every end marker a Put sent
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
        field, body, ended = None, bytearray(), False
        async for r in reqs:
            if r.WhichOneof("part") == "header":
                field = r.header.field
            elif r.WhichOneof("part") == "end":
                self.put_ends.append(r.end.size)
                ended = r.end.size == len(body)
            else:
                self.put_chunks.append(len(r.chunk))
                body += r.chunk
        if len(body) > 262144:
            raise GRPCError(Status.INVALID_ARGUMENT, "w17.files: too large",
                            [error_details_pb2.ErrorInfo(domain=files.DOMAIN, reason="FILE_TOO_LARGE",
                                                         metadata={"field": field, "limit_bytes": "262144"})])
        if not ended:  # as kvfsgrpc: only the end marker commits
            raise GRPCError(Status.INVALID_ARGUMENT, "w17.files: no end marker",
                            [error_details_pb2.ErrorInfo(domain=files.DOMAIN, reason="BAD_REQUEST")])
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
    companion.put_ends.clear()
    handle = await store.put(ctx, "conformance.v1.NoteFile.file", body)
    assert companion.put_chunks == [64 << 10, 64 << 10, 64 << 10, 8 << 10]
    assert companion.put_ends == [200 << 10], "the end marker, last, with the body's size"
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
    companion.put_ends.clear()
    handle = await store.put(Context(), "NoteFile.file", b"")
    assert companion.put_chunks == [] and await store.read(Context(), handle) == b""
    assert companion.put_ends == [0]


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

    stored = len(companion.objects)
    companion.put_ends.clear()
    with pytest.raises(OSError, match="the author's file is gone"):
        await store.put(Context(), "NoteFile.file", broken())
    await asyncio.sleep(0.1)
    # A failed body never sends the end marker, so the companion stores nothing.
    assert companion.put_ends == [] and len(companion.objects) == stored


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
