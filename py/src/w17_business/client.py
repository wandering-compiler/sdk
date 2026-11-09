"""gRPC clients to the storage tier (and any other tier), built from descriptors.

Every call does what Go's generated inter-tier client does (sdk/go/core/grpcx
``DialOpts``):

- relays the verified principal (``principal.forward_to_outgoing``) — exact key
  set, replace semantics;
- sends the context's outgoing metadata (transaction routing, paging the author
  forwarded) and its deadline as ``grpc-timeout``; a context already done is
  refused before anything is sent, as a gRPC client refuses it;
- injects W3C trace context when OpenTelemetry is on;
- retries ``UNAVAILABLE`` — and a connection that failed in transport, which
  grpc-go reports as ``UNAVAILABLE`` — and nothing else: 3 attempts, backoff
  0.1s × 2 up to 1s with jitter, under the channel's retry throttle (100 tokens,
  0.1 per success), the service config ``grpcclient.DefaultServiceConfig`` sets;
- keeps the connection alive (ping every 30s, 10s timeout, also without calls);
- dials TLS per ``W17_INTERNAL_TLS``.

A transport failure that survives the retries is raised as
``GRPCError(UNAVAILABLE)`` and a deadline as ``GRPCError(DEADLINE_EXCEEDED)``,
so a caller handles one exception type, as with grpc-go.

The business companion is dialled with a ``CompanionConn`` of its own (never
the storage ``Conn``, even at the same address): the same principal relay,
trace context, deadline, keepalive and TLS, but ONE attempt per call and none
of the storage-transaction or paging headers (see the class).
"""

from __future__ import annotations

import asyncio
import random
from collections.abc import AsyncIterable, AsyncIterator, Iterable
from typing import Any

from grpclib.client import Channel
from grpclib.config import Configuration
from grpclib.const import Cardinality, Status
from grpclib.exceptions import GRPCError, StreamTerminatedError
from grpclib.metadata import Deadline

from . import observability, paging, principal, tlsconfig
from .context import Context, Metadata
from .descriptors import MethodInfo, Pool, default_pool
from .errors import StatusDetailsCodec

__all__ = ["RetryPolicy", "Conn", "CompanionConn", "ServiceClient", "TxCoordinator", "dial", "parse_addr"]


class RetryPolicy:
    """grpcclient.DefaultServiceConfig, applied by hand: grpclib has no
    service-config retries."""

    max_attempts = 3
    initial_backoff = 0.1
    max_backoff = 1.0
    multiplier = 2.0
    throttle_max_tokens = 100.0
    throttle_token_ratio = 0.1

    def backoff(self, retry: int) -> float:
        # gRPC A6: the n-th retry waits random(0, min(initial * mult^(n-1), max)).
        cap = min(self.initial_backoff * (self.multiplier ** (retry - 1)), self.max_backoff)
        return random.uniform(0, cap)


def parse_addr(addr: str) -> tuple[str, int]:
    """``host:port`` / ``[v6]:port`` / ``:port`` (localhost)."""
    if addr.startswith("["):
        host, _, rest = addr[1:].partition("]")
        port = rest.lstrip(":")
    else:
        host, _, port = addr.rpartition(":")
    if not port.isdigit():
        raise ValueError(f"address {addr!r} has no port")
    return host or "localhost", int(port)


def _channel_config() -> Configuration:
    # grpcclient.DefaultClientKeepalive: Time 30s, Timeout 10s, PermitWithoutStream.
    return Configuration(
        _keepalive_time=30.0,
        _keepalive_timeout=10.0,
        _keepalive_permit_without_calls=True,
        _http2_max_pings_without_data=0,
    )


def _retryable(e: BaseException) -> bool:
    if isinstance(e, GRPCError):
        return e.status == Status.UNAVAILABLE
    # A deadline is NOT retryable: TimeoutError is an OSError subclass, and
    # replaying a call whose time budget is spent would wait twice and may
    # repeat a write the server already applied.
    if isinstance(e, TimeoutError):
        return False
    return isinstance(e, (ConnectionError, StreamTerminatedError, OSError))


def _as_status(e: BaseException) -> BaseException:
    if isinstance(e, GRPCError):
        return e
    if isinstance(e, TimeoutError):
        return GRPCError(Status.DEADLINE_EXCEEDED, "context deadline exceeded")
    if isinstance(e, (ConnectionError, StreamTerminatedError, OSError)):
        return GRPCError(Status.UNAVAILABLE, f"connection error: {e}")
    return e


class Conn:
    """One channel to one address, shared by every client on it."""

    def __init__(self, addr: str, *, tls: bool | None = None, environ: dict[str, str] | None = None) -> None:
        host, port = parse_addr(addr)
        ssl_ctx = tlsconfig.client_context(environ) if tls is not False else None
        if tls is True and ssl_ctx is None:
            ssl_ctx = tlsconfig.client_context({tlsconfig.ENV_INTERNAL_TLS: "on", **(environ or {})})
        self.addr = addr
        self.channel = Channel(
            host,
            port,
            ssl=ssl_ctx,
            config=_channel_config(),
            status_details_codec=StatusDetailsCodec(),
        )
        self.policy = RetryPolicy()
        self._tokens = self.policy.throttle_max_tokens

    def close(self) -> None:
        self.channel.close()

    # retryThrottling (gRPC A6): a failure costs a token, a success returns
    # token_ratio; retries are allowed only above half the bucket.
    def _on_failure(self) -> bool:
        self._tokens = max(0.0, self._tokens - 1)
        return self._tokens > self.policy.throttle_max_tokens / 2

    def _on_success(self) -> None:
        self._tokens = min(self.policy.throttle_max_tokens, self._tokens + self.policy.throttle_token_ratio)

    def _prepare(self, ctx: Context) -> tuple[list[tuple[str, str]], Deadline | None]:
        if ctx.done():
            st = Status.DEADLINE_EXCEEDED if ctx.err() == "context deadline exceeded" else Status.CANCELLED
            raise GRPCError(st, ctx.err())
        ctx = principal.forward_to_outgoing(ctx)
        pairs = observability.inject_trace(ctx.outgoing.pairs())
        # Public API only: grpclib measures a Deadline on time.monotonic(), as ctx does.
        rem = ctx.time_remaining()
        deadline = Deadline.from_timeout(max(rem, 0.0)) if rem is not None else None
        return pairs, deadline

    async def unary(self, ctx: Context, m: MethodInfo, req: Any, *, timeout: float | None = None) -> tuple[Any, Metadata]:
        """One unary call with the retry policy; returns (reply, response headers)."""
        if timeout is not None:
            ctx = ctx.with_timeout(timeout)
        reply, header = await self._unary(ctx, m, req)
        # A call made with paging.forward relays the page position storage
        # reported onto the business method's response (Go's
        # paging.RelayPositionUnaryInterceptor).
        paging.relay_position(ctx, header)
        return reply, header

    async def _unary(self, ctx: Context, m: MethodInfo, req: Any) -> tuple[Any, Metadata]:
        attempt = 0
        while True:
            attempt += 1
            pairs, deadline = self._prepare(ctx)
            # gRPC A6: a call is COMMITTED once the server's response headers
            # arrived, and a committed call is never retried — the server has
            # begun answering, so it may have acted (a Commit whose reply was
            # then lost must reach the outcome probe, not run twice). A
            # trailers-only answer (UNAVAILABLE before any header) is not
            # committed and is retried.
            committed = False
            try:
                async with self.channel.request(
                    m.path, Cardinality.UNARY_UNARY, m.request, m.response, deadline=deadline, metadata=pairs
                ) as stream:
                    await stream.send_message(req, end=True)
                    await stream.recv_initial_metadata()
                    committed = True
                    reply = await stream.recv_message()
                    header = Metadata(list(stream.initial_metadata.items()) if stream.initial_metadata else None)
                    await stream.recv_trailing_metadata()
                if reply is None:
                    raise GRPCError(Status.UNKNOWN, f"{m.path}: no response message")
                self._on_success()
                return reply, header
            except asyncio.CancelledError:
                raise
            except Exception as e:
                if committed or not _retryable(e):
                    raise _as_status(e) from e
                allowed = self._on_failure()
                if attempt >= self.policy.max_attempts or not allowed or ctx.done():
                    raise _as_status(e) from e
                await asyncio.sleep(self.policy.backoff(attempt))

    async def server_stream(self, ctx: Context, m: MethodInfo, req: Any) -> AsyncIterator[Any]:
        pairs, deadline = self._prepare(ctx)
        try:
            async with self.channel.request(
                m.path, Cardinality.UNARY_STREAM, m.request, m.response, deadline=deadline, metadata=pairs
            ) as stream:
                await stream.send_message(req, end=True)
                async for reply in stream:
                    yield reply
        except (TimeoutError, ConnectionError, StreamTerminatedError, OSError) as e:
            raise _as_status(e) from e

    async def client_stream(self, ctx: Context, m: MethodInfo, reqs: Iterable[Any] | AsyncIterable[Any]) -> Any:
        pairs, deadline = self._prepare(ctx)
        try:
            async with self.channel.request(
                m.path, Cardinality.STREAM_UNARY, m.request, m.response, deadline=deadline, metadata=pairs
            ) as stream:
                await _send_all(stream, reqs)
                return await stream.recv_message()
        except (TimeoutError, ConnectionError, StreamTerminatedError, OSError) as e:
            raise _as_status(e) from e

    async def bidi_stream(self, ctx: Context, m: MethodInfo, reqs: Iterable[Any] | AsyncIterable[Any]) -> AsyncIterator[Any]:
        pairs, deadline = self._prepare(ctx)
        try:
            async with self.channel.request(
                m.path, Cardinality.STREAM_STREAM, m.request, m.response, deadline=deadline, metadata=pairs
            ) as stream:
                sender = asyncio.ensure_future(_send_all(stream, reqs))
                try:
                    async for reply in stream:
                        yield reply
                    await sender
                finally:
                    if not sender.done():
                        sender.cancel()
        except (TimeoutError, ConnectionError, StreamTerminatedError, OSError) as e:
            raise _as_status(e) from e


# Never sent to the companion: a file or an HTTP call is not part of a storage
# transaction, and a host that adopts transactions from metadata must not see
# one; the page position is a storage call's.
_COMPANION_STRIPPED = ("w17-tx-id", "w17-conn-id")
_COMPANION_STRIPPED_PREFIX = "x-w17-paging-"


def companion_pairs(pairs: list[tuple[str, str]]) -> list[tuple[str, str]]:
    """``pairs`` without ``w17-tx-id``, ``w17-conn-id`` and ``x-w17-paging-*``."""
    out = []
    for k, v in pairs:
        lk = k.lower()
        if lk in _COMPANION_STRIPPED or lk.startswith(_COMPANION_STRIPPED_PREFIX):
            continue
        out.append((k, v))
    return out


class CompanionConn(Conn):
    """The connection to the business companion (egress clients, file storage).

    A ``Conn`` with two differences:

    - **no retries**: one attempt, ``UNAVAILABLE`` is raised to the author. The
      egress runtime already retried what is safe to retry, and its
      ``UNAVAILABLE`` (an upstream 503) arrives trailers-only, which the
      storage policy would replay — a second POST; a ``Put`` stream cannot be
      replayed at all;
    - **no transaction or paging headers**: ``w17-tx-id``, ``w17-conn-id`` and
      ``x-w17-paging-*`` are stripped from every call, and no page position is
      relayed back.

    The principal, trace context, ``w17-language`` / ``w17-format-locale`` /
    ``w17-label-*`` and the deadline go out as on a storage call."""

    def _prepare(self, ctx: Context) -> tuple[list[tuple[str, str]], Deadline | None]:
        pairs, deadline = super()._prepare(ctx)
        return companion_pairs(pairs), deadline

    async def unary(self, ctx: Context, m: MethodInfo, req: Any, *, timeout: float | None = None) -> tuple[Any, Metadata]:
        if timeout is not None:
            ctx = ctx.with_timeout(timeout)
        pairs, deadline = self._prepare(ctx)
        try:
            async with self.channel.request(
                m.path, Cardinality.UNARY_UNARY, m.request, m.response, deadline=deadline, metadata=pairs
            ) as stream:
                await stream.send_message(req, end=True)
                reply = await stream.recv_message()
                header = Metadata(list(stream.initial_metadata.items()) if stream.initial_metadata else None)
                await stream.recv_trailing_metadata()
        except asyncio.CancelledError:
            raise
        except Exception as e:
            raise _as_status(e) from e
        if reply is None:
            raise GRPCError(Status.UNKNOWN, f"{m.path}: no response message")
        return reply, header

    async def client_stream(self, ctx: Context, m: MethodInfo, reqs: Iterable[Any] | AsyncIterable[Any]) -> Any:
        pairs, deadline = self._prepare(ctx)
        try:
            async with self.channel.request(
                m.path, Cardinality.STREAM_UNARY, m.request, m.response, deadline=deadline, metadata=pairs
            ) as stream:
                try:
                    await _send_all(stream, reqs)
                except (ConnectionError, StreamTerminatedError, OSError):
                    # The server answered before the requests ran out (a body
                    # too large, an unknown field): its status follows, and is
                    # the error to raise — Go's Send returns io.EOF then.
                    pass
                return await stream.recv_message()
        except asyncio.CancelledError:
            raise
        except Exception as e:
            raise _as_status(e) from e


async def _send_all(stream, reqs) -> None:
    await stream.send_request()
    if hasattr(reqs, "__aiter__"):
        async for r in reqs:
            await stream.send_message(r)
    else:
        for r in reqs:
            await stream.send_message(r)
    await stream.end()


class ServiceClient:
    """A client of one gRPC service; its methods are the RPCs, by exact name:

        note = await clients.storage("notes.v1.NoteStorage").CreateNote(ctx, req)

    Unary → awaitable reply; server-streaming → async iterator; client-streaming
    → awaitable taking an iterable; bidi → async iterator taking an iterable."""

    def __init__(self, conn: Conn, service: str, pool: Pool | None = None) -> None:
        self._conn = conn
        self._service = service
        self._methods = {m.name: m for m in (pool or default_pool()).methods(service)}

    @property
    def service(self) -> str:
        return self._service

    def method_info(self, name: str) -> MethodInfo:
        return self._methods[name]

    def __getattr__(self, name: str):
        m = self.__dict__.get("_methods", {}).get(name)
        if m is None:
            raise AttributeError(f"{self._service} has no rpc {name!r}")
        conn = self._conn
        if not m.client_streaming and not m.server_streaming:

            async def call(ctx: Context, req: Any, *, timeout: float | None = None) -> Any:
                reply, _ = await conn.unary(ctx, m, req, timeout=timeout)
                return reply

        elif m.server_streaming and not m.client_streaming:

            def call(ctx: Context, req: Any):  # type: ignore[misc]
                return conn.server_stream(ctx, m, req)

        elif m.client_streaming and not m.server_streaming:

            async def call(ctx: Context, reqs):  # type: ignore[misc]
                return await conn.client_stream(ctx, m, reqs)

        else:

            def call(ctx: Context, reqs):  # type: ignore[misc]
                return conn.bidi_stream(ctx, m, reqs)

        call.__name__ = name
        call.__qualname__ = f"{self._service}.{name}"
        return call


class TxCoordinator:
    """The ``W17DistributedTransaction`` client distx drives (``distx.TxClient``)."""

    SERVICE = "w17.common.distx.W17DistributedTransaction"

    def __init__(self, conn: Conn) -> None:
        self._conn = conn
        ms = {m.name: m for m in default_pool().methods(self.SERVICE)}
        self._begin, self._commit, self._rollback = ms["Begin"], ms["Commit"], ms["Rollback"]

    async def begin(self, ctx: Context, req: Any):
        return await self._conn.unary(ctx, self._begin, req)

    async def commit(self, ctx: Context, req: Any):
        reply, _ = await self._conn.unary(ctx, self._commit, req)
        return reply

    async def rollback(self, ctx: Context, req: Any):
        reply, _ = await self._conn.unary(ctx, self._rollback, req)
        return reply


def dial(addr: str, *, tls: bool | None = None) -> Conn:
    return Conn(addr, tls=tls)
