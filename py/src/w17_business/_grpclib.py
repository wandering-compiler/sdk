"""The one place the SDK reaches past grpclib's public API.

grpclib's ``Server`` exposes no listening socket and no graceful stop, so the
runtime reads ``Server._server`` (the ``asyncio.Server``) for the bound port
and to stop accepting before the drain. That is private: pyproject pins grpclib
to one exact version, and this module checks the attribute right after the
server starts, so a grpclib that renamed it stops the boot naming the cause —
rather than the drain failing at the first SIGTERM, or the port reading as 0.

Deadlines go through grpclib's public ``Deadline.from_timeout`` /
``time_remaining``; nothing else here is private.
"""

from __future__ import annotations

import asyncio

import grpclib
from grpclib.server import Server

__all__ = ["GrpclibAPIError", "listener"]


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
