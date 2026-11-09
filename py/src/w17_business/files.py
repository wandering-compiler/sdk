"""File storage: the domain's upload connections, served by the business companion.

A Go business tier stores and reads files through ``sdk/go/lib/kvfs``
in-process; this tier calls the same Go code in the companion over
``w17.common.files.W17Files``, so a file is laid out, named and read back
exactly as in Go. ``clients.store("app-exports")`` is one connection::

    store = clients.store("app-exports")
    handle = await store.put(ctx, "app.example.NoteExport.file", body)  # kvfs.Put
    data = await store.read(ctx, handle)                                # whole object
    async for chunk in store.open(ctx, handle): ...                     # streamed
    info = await store.stat(ctx, handle)                                # FileInfo
    await store.delete(ctx, handle)                                     # writable only

The handle is the value the field's column holds — byte for byte what Go's
``kvfs.Put`` returns; store it as it is and pass column values back as they
are. A file is not part of a storage transaction (as in Go): a file put inside
one that then rolls back stays, so delete it yourself when the unit of work
fails.

``is_not_found(err)`` is Go's ``errors.Is(err, kvfs.ErrNotFound)``.
"""

from __future__ import annotations

from collections.abc import AsyncIterable, AsyncIterator, Iterable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from .client import CompanionConn
from .context import Context
from .descriptors import Pool, default_pool
from .errors import error_info
from .manifest import FileConnection, FilePlacement

__all__ = ["SERVICE", "DOMAIN", "CHUNK_SIZE", "FileInfo", "FileStore", "is_not_found", "resolve_field"]

SERVICE = "w17.common.files.W17Files"
DOMAIN = "w17.files"  # the ErrorInfo domain of the companion's file errors
CHUNK_SIZE = 64 << 10  # the size of the chunks put sends

Body = bytes | bytearray | memoryview | Iterable[bytes] | AsyncIterable[bytes]


@dataclass(frozen=True)
class FileInfo:
    """A stored object's size and modification time (Go's ``kvfs.Info``)."""

    size: int
    mod_time: datetime


def is_not_found(err: BaseException | None) -> bool:
    """True for the companion's "no stored file has this handle"
    (``FILE_NOT_FOUND``) — Go's ``errors.Is(err, kvfs.ErrNotFound)``."""
    info = error_info(err)
    return info is not None and info.domain == DOMAIN and info.reason == "FILE_NOT_FOUND"


def resolve_field(field: str, placements: Iterable[FilePlacement]) -> FilePlacement:
    """The placement of ``field``: its full proto name, or ``Message.field``
    when that names exactly one placement."""
    ps = list(placements)
    name = field.lstrip(".")
    for p in ps:
        if p.field == name:
            return p
    matches = [p for p in ps if p.field.endswith("." + name)] if "." in name else []
    if len(matches) == 1:
        return matches[0]
    known = ", ".join(sorted(p.field for p in ps)) or "none"
    if not matches:
        raise KeyError(f"files: {field!r} is not a field this tier writes files for (have: {known})")
    raise KeyError(f"files: {field!r} is ambiguous ({', '.join(sorted(p.field for p in matches))})")


class _BodyError(Exception):
    """An error the author's body raised, carried past the stream's own."""

    def __init__(self, err: BaseException) -> None:
        super().__init__(str(err))
        self.err = err


class FileStore:
    """One upload connection's store, as the companion serves it."""

    def __init__(self, conn: CompanionConn, connection: FileConnection, placements: Iterable[FilePlacement],
                 pool: Pool | None = None) -> None:
        self._conn = conn
        self._connection = connection
        self._placements = tuple(placements)  # every placement of the manifest
        pool = pool or default_pool()
        ms = {m.name: m for m in pool.methods(SERVICE)}
        self._put, self._open, self._stat, self._delete = ms["Put"], ms["Open"], ms["Stat"], ms["Delete"]
        self._put_req = self._put.request
        self._header = pool.message("w17.common.files.PutHeader")

    @property
    def name(self) -> str:
        return self._connection.name

    @property
    def writable(self) -> bool:
        """True when this tier writes the connection (``upload.writer: BUSINESS``);
        a connection the gateway writes is read-only here."""
        return self._connection.writable

    def _require_writable(self, what: str) -> None:
        if not self.writable:
            raise PermissionError(
                f"files: {what} on {self.name}: the gateway writes this connection; this tier only reads it")

    async def put(self, ctx: Context, field: str, body: Body) -> str:
        """Store ``body`` as a new object of ``field`` (full proto name, or
        ``Message.field`` when unambiguous) and return its handle. ``body`` is
        bytes-like, or an iterable / async iterable of bytes; it is sent in
        64 KiB chunks."""
        p = resolve_field(field, self._placements)
        if p.connection != self.name:
            raise KeyError(f"files: {p.field} is stored on {p.connection}, not on {self.name}")
        self._require_writable(f"put {p.field}")
        try:
            reply = await self._conn.client_stream(ctx, self._put, self._requests(p.field, body))
        except _BodyError as e:
            raise e.err from None
        return reply.handle

    async def _requests(self, field: str, body: Body) -> AsyncIterator[Any]:
        yield self._put_req(header=self._header(field=field))
        try:
            async for chunk in _chunks(body):
                yield self._put_req(chunk=chunk)
        except Exception as e:  # noqa: BLE001 — the author's, re-raised by put
            raise _BodyError(e) from e

    async def open(self, ctx: Context, handle: str) -> AsyncIterator[bytes]:
        """The stored object's bytes, as chunks of at most 64 KiB."""
        req = self._open.request(connection=self.name, handle=handle)
        async for msg in self._conn.server_stream(ctx, self._open, req):
            if msg.chunk:
                yield bytes(msg.chunk)

    async def read(self, ctx: Context, handle: str) -> bytes:
        """The whole stored object, in memory."""
        return b"".join([c async for c in self.open(ctx, handle)])

    async def stat(self, ctx: Context, handle: str) -> FileInfo:
        reply, _ = await self._conn.unary(ctx, self._stat, self._stat.request(connection=self.name, handle=handle))
        ts = reply.mod_time
        mod = datetime.fromtimestamp(ts.seconds, tz=UTC).replace(microsecond=ts.nanos // 1000)
        return FileInfo(size=int(reply.size), mod_time=mod)

    async def delete(self, ctx: Context, handle: str) -> None:
        """Remove a stored object; a missing one is not an error."""
        self._require_writable("delete")
        await self._conn.unary(ctx, self._delete, self._delete.request(connection=self.name, handle=handle))


async def _chunks(body: Body) -> AsyncIterator[bytes]:
    """body re-chunked to CHUNK_SIZE."""
    if isinstance(body, (bytes, bytearray, memoryview)):
        mv = memoryview(body).cast("B")
        for i in range(0, len(mv), CHUNK_SIZE):
            yield bytes(mv[i : i + CHUNK_SIZE])
        return
    buf = bytearray()
    if hasattr(body, "__aiter__"):
        async for part in body:  # type: ignore[union-attr]
            buf += _bytes(part)
            while len(buf) >= CHUNK_SIZE:
                yield bytes(buf[:CHUNK_SIZE])
                del buf[:CHUNK_SIZE]
    elif isinstance(body, Iterable) and not isinstance(body, str):
        for part in body:
            buf += _bytes(part)
            while len(buf) >= CHUNK_SIZE:
                yield bytes(buf[:CHUNK_SIZE])
                del buf[:CHUNK_SIZE]
    else:
        raise TypeError(f"files: a body is bytes or an (async) iterable of bytes, not {type(body).__name__}")
    if buf:
        yield bytes(buf)


def _bytes(part: Any) -> bytes | bytearray | memoryview:
    if isinstance(part, (bytes, bytearray, memoryview)):
        return part
    raise TypeError(f"files: a body yields bytes, not {type(part).__name__}")
