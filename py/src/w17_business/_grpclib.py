"""The one place the SDK reaches past grpclib's public API.

grpclib's ``Server`` exposes no listening socket and no graceful stop, so the
runtime reads ``Server._server`` (the ``asyncio.Server``) for the bound port
and to stop accepting before the drain. That is private: pyproject pins grpclib
to one exact version, and this module checks the attribute right after the
server starts, so a grpclib that renamed it stops the boot naming the cause —
rather than the drain failing at the first SIGTERM, or the port reading as 0.

Deadlines go through grpclib's public ``Deadline.from_timeout`` /
``time_remaining``.

grpclib also has no maximum received message size (Go, grpc-js and tonic cap
it at 4 MiB): its ``recv_message`` reads a message's 5-byte prefix and then
buffers however many bytes the prefix names. ``bound_received`` wraps the
served stream's private ``_stream`` (the HTTP/2 stream it reads DATA from) so
a prefix past the limit is refused before the body is read. Checked per call,
like the listener: a grpclib without the attribute refuses the call naming
the cause.
"""

from __future__ import annotations

import asyncio
from typing import Any

import grpclib
from grpclib.const import Status
from grpclib.exceptions import GRPCError
from grpclib.server import Server

__all__ = ["MAX_RECV_MESSAGE", "GrpclibAPIError", "bound_received", "listener"]

MAX_RECV_MESSAGE = 4 * 1024 * 1024
"""The largest message a served method receives: grpc-go's default."""


class GrpclibAPIError(RuntimeError):
    """The installed grpclib lacks what the SDK reads from it."""


def listener(server: Server) -> asyncio.AbstractServer | None:
    """The asyncio server under a started grpclib ``Server`` (None before
    start). Raises GrpclibAPIError when grpclib no longer keeps it there."""
    try:
        aserver = vars(server)["_server"]
    except (KeyError, TypeError):
        raise GrpclibAPIError(
            f"grpclib {getattr(grpclib, '__version__', '?')}: Server has no _server attribute — the SDK "
            "reads it for the bound port and the drain; install the grpclib version pyproject pins") from None
    if aserver is not None and not isinstance(aserver, asyncio.AbstractServer):
        raise GrpclibAPIError(
            f"grpclib {getattr(grpclib, '__version__', '?')}: Server._server is a {type(aserver).__name__}, "
            "not an asyncio server; install the grpclib version pyproject pins")
    return aserver


class _BoundedData:
    """The HTTP/2 stream a served ``Stream`` reads from, refusing a message
    whose length prefix exceeds the limit. grpclib's ``recv_message`` asks for
    exactly two reads per message — the 5-byte prefix, then the body — so
    every other read is a prefix."""

    __slots__ = ("_inner", "_limit", "_prefix_next")

    def __init__(self, inner: Any, limit: int) -> None:
        self._inner = inner
        self._limit = limit
        self._prefix_next = True

    def __getattr__(self, name: str) -> Any:
        return getattr(self._inner, name)

    async def recv_data(self, size: int) -> bytes:
        data = await self._inner.recv_data(size)
        if not self._prefix_next:
            self._prefix_next = True
            return data
        if len(data) == 5:
            self._prefix_next = False
            n = int.from_bytes(data[1:5], "big")
            if n > self._limit:
                raise GRPCError(Status.RESOURCE_EXHAUSTED,
                                f"grpc: received message larger than max ({n} vs. {self._limit})")
        return data


def bound_received(stream: Any, limit: int = MAX_RECV_MESSAGE) -> None:
    """Make the served grpclib ``stream`` refuse a received message larger than
    ``limit`` with RESOURCE_EXHAUSTED before its body is buffered."""
    try:
        inner = vars(stream)["_stream"]
    except (KeyError, TypeError):
        raise GrpclibAPIError(
            f"grpclib {getattr(grpclib, '__version__', '?')}: server Stream has no _stream attribute — the SDK "
            "reads it to bound a received message; install the grpclib version pyproject pins") from None
    if not isinstance(inner, _BoundedData):
        stream._stream = _BoundedData(inner, limit)
