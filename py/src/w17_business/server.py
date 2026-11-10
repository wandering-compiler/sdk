"""Serving business methods: the registry, the middleware, the grpclib mapping.

A handler is a class whose methods are named after the RPCs — the EXACT proto
name, as in Go (``CreateNote``, not ``create_note``), so one name means one
thing in the proto, the gateway, the logs and the code:

    class NoteService:
        async def CreateNote(self, ctx, req):          # unary
            ...
        async def WatchNotes(self, ctx, req):          # server-streaming:
            yield ...                                  #   an async generator
        async def Upload(self, ctx, requests):         # client-streaming:
            async for r in requests: ...               #   an async iterator in
        async def Chat(self, ctx, requests):           # bidi: both
            async for r in requests: yield ...

``registry.register("notes.v1.NoteService", NoteService(...))`` checks every RPC
of the service against the class and refuses (``RegistrationError``, which stops
the boot) when one is missing or is not an ``async def``.

Every unary method runs through the same chain, over the wire AND through
``registry.self(...)``:

1. the author's method;
2. the emit middleware, when the manifest lists an emit for it: after the
   method returns, the event is published — or, inside a transaction, parked
   until the root commits and dropped if it rolls back (``txscope``).

**Compose your own methods through ``registry.self(Service)``**, which returns
the served service with its middleware. A direct call on your class skips the
emit, and nothing fails — exactly the Go rule.

Over the wire the runtime adds, outside that chain: the context (incoming
metadata, deadline, method), an OpenTelemetry server span, and the mapping of
exceptions to statuses (see ``errors``). No emit on a streaming method, as in Go.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import time
from collections.abc import AsyncIterator, Callable
from typing import Any

import grpclib.const
from grpclib.const import Cardinality, Status
from grpclib.exceptions import GRPCError, ProtocolError

from . import _grpclib, distx, events, observability, paging, txscope
from .config import ConfigError
from .context import Context, Metadata
from .descriptors import MethodInfo, Pool
from .distx import DistxError
from .errors import UserError
from .manifest import Emit, Invalidate
from .principal import NoPrincipalError

__all__ = ["Registry", "RegistrationError", "Served"]

log = logging.getLogger("w17_business")

# Errors the SDK raises that Go would RETURN (not panic with): answered UNKNOWN
# with their message, as grpc-go answers a plain returned error.
_RETURNED = (DistxError, NoPrincipalError, ConfigError)


class RegistrationError(Exception):
    """The registry cannot serve what it was asked to; stops the boot."""


def _short(service: str) -> str:
    return service.rsplit(".", 1)[-1]


class _Method:
    """One served RPC: the author's bound method plus the middleware."""

    def __init__(self, info: MethodInfo, fn: Callable[..., Any], emit: Emit | None,
                 invalidates: tuple[Invalidate, ...], registry: Registry) -> None:
        self.info = info
        self.fn = fn
        self.emit = emit
        self.invalidates = invalidates
        self.registry = registry
        self.builder = events.EnvelopeBuilder(registry.pool, emit) if emit is not None else None

    async def invoke_unary(self, ctx: Context, req: Any) -> Any:
        resp = await self.fn(ctx, req)
        if resp is None:
            raise GRPCError(Status.INTERNAL, f"{self.info.path}: handler returned no response")
        if self.builder is not None or self.invalidates:
            await self._emit(ctx, req, resp)
        return resp

    async def _emit(self, ctx: Context, req: Any, resp: Any) -> None:
        """The Go emit wrapper: after success, publish — or, inside a
        transaction, park until the root commits (dropped on rollback). The
        emit goes first, the invalidates after it, as the Go helper orders
        them."""
        reg, emit, builder = self.registry, self.emit, self.builder
        path = self.info.path
        deferred = ctx.without_cancel()

        async def publish(c: Context) -> None:
            if emit is not None and builder is not None:
                try:
                    env = builder.build(c, req, resp)
                except Exception as e:  # noqa: BLE001 — an emit never fails the RPC
                    observability.report_error(c, RuntimeError(f"eventbus: build the {path} event: {e!r}"))
                else:
                    await events.fire(reg.dispatcher, c, emit.channel, emit.topic, env, path)
            for inv in self.invalidates:
                for ev in events.invalidate_events(reg.pool, inv, c):
                    await events.fire(reg.dispatcher, c, inv.channel, inv.topic, ev, path)

        out = txscope.after_commit(ctx, lambda: publish(deferred))
        if out in (txscope.Outcome.NO_SCOPE, txscope.Outcome.ALREADY_COMMITTED):
            # The write behind it has landed: a caller cancelled now (a client
            # hang-up, a deadline) must not take the event with it.
            await txscope.shielded(publish(deferred))
        elif out is txscope.Outcome.DISCARDED:
            observability.report_error(
                ctx, "txregistry: dropped an announcement made after its in-process transaction rolled back")


class Served:
    """A served service: its RPCs, with the middleware, by exact name.

    ``registry.register`` returns it and ``registry.self`` finds it again:

        note = await self.svc.CreateNote(ctx, req)   # emits, as over the wire
    """

    def __init__(self, service: str, methods: dict[str, _Method]) -> None:
        self.service = service
        self._methods = methods

    def __getattr__(self, name: str):
        m = self.__dict__.get("_methods", {}).get(name)
        if m is None:
            raise AttributeError(f"{self.__dict__.get('service')} has no rpc {name!r}")
        if not m.info.client_streaming and not m.info.server_streaming:
            return m.invoke_unary
        return m.fn

    def __mapping__(self) -> dict[str, grpclib.const.Handler]:
        return {m.info.path: _handler(m, self) for m in self._methods.values()}


class Registry:
    """What ``register_business(cfg, registry, clients)`` registers into.

    ``services``, when given (the manifest's list), is what the registry may
    serve: registering any other service is refused. ``emits`` and
    ``invalidates`` are keyed by the method's gRPC path."""

    def __init__(
        self,
        pool: Pool,
        emits: tuple[Emit, ...] = (),
        dispatcher: events.Dispatcher | None = None,
        invalidates: tuple[Invalidate, ...] = (),
        services: tuple[str, ...] | None = None,
    ) -> None:
        self.pool = pool
        self.dispatcher = dispatcher
        self._emits = {e.method: e for e in emits}
        self._invalidates: dict[str, list[Invalidate]] = {}
        for inv in invalidates:
            self._invalidates.setdefault(inv.method, []).append(inv)
        self._allowed = None if services is None else frozenset(s.lstrip(".") for s in services)
        self._served: dict[str, Served] = {}
        self._shutdowns: list[Callable[[Context], Any]] = []
        self.draining = False
        self._inflight = 0
        self._idle = asyncio.Event()
        self._idle.set()

    # --- registration ------------------------------------------------------

    def _resolve_service(self, service: str | type) -> str:
        name = service if isinstance(service, str) else getattr(service, "__w17_service__", service.__name__)
        name = name.lstrip(".")
        try:
            return self.pool.service(name).full_name
        except KeyError:
            pass
        matches = sorted(
            {sd.full_name for f in self.pool.files for sd in self.pool.file(f).services_by_name.values()
             if sd.name == name}
        )
        if len(matches) == 1:
            return matches[0]
        if not matches:
            raise RegistrationError(f"registry: no service {name!r} in the descriptor set")
        raise RegistrationError(f"registry: {name!r} is ambiguous ({', '.join(matches)}); use the full name")

    def register(self, service: str | type, impl: Any) -> Served:
        """Serve ``impl`` as ``service`` (full name, or short when unambiguous).

        Returns the served service — keep it to compose your own methods
        through the middleware (or find it again with ``self``)."""
        full = self._resolve_service(service)
        if self._allowed is not None and full not in self._allowed:
            raise RegistrationError(
                f"registry: {full} is not a business service of this tier (the manifest lists: "
                f"{', '.join(sorted(self._allowed)) or 'none'})")
        if full in self._served:
            raise RegistrationError(f"registry: {full} is already registered")
        methods: dict[str, _Method] = {}
        missing, not_async = [], []
        for info in self.pool.methods(full):
            fn = getattr(impl, info.name, None)
            if fn is None or not callable(fn):
                missing.append(info.name)
                continue
            streaming_out = info.server_streaming
            ok = inspect.isasyncgenfunction(fn) if streaming_out else inspect.iscoroutinefunction(fn)
            if not ok:
                not_async.append(f"{info.name} (want {'an async generator' if streaming_out else 'async def'})")
                continue
            emit = self._emits.get(info.path)
            invs = tuple(self._invalidates.get(info.path, ()))
            if (emit is not None or invs) and (info.client_streaming or info.server_streaming):
                raise RegistrationError(f"registry: {info.path} is streaming and cannot emit an event")
            methods[info.name] = _Method(info, fn, emit, invs, self)
        cls = type(impl).__name__
        if missing:
            raise RegistrationError(
                f"registry: {cls} does not implement {full}: missing method(s) {', '.join(missing)} "
                "(handler methods are named exactly as the RPCs)")
        if not_async:
            raise RegistrationError(f"registry: {cls} for {full}: {', '.join(not_async)}")
        served = Served(full, methods)
        self._served[full] = served
        return served

    def self(self, service: str | type) -> Served:
        """The served ``service`` — call your own methods through this, never
        directly on your class, or their events are skipped.

        It is Go's direct ``s.self.Method(ctx, req)``: the callee gets the ctx
        you pass UNCHANGED — your incoming metadata, your transaction (it
        joins), and whatever you put on it. So pass a ``paging.forward`` ctx
        only to the storage call that returns the page, never here (the
        callee's own calls would carry the page and relay their position onto
        your response); and ``principal.act_as`` on a ctx passed here is not
        what the callee's principal readers see — it sets the OUTGOING side,
        they read the incoming one — exactly as in Go."""
        full = self._resolve_service(service)
        s = self._served.get(full)
        if s is None:
            raise RegistrationError(f"registry: {full} is not registered (register it before composing it)")
        return s

    def register_shutdown(self, fn: Callable[[Context], Any]) -> None:
        """A drain hook: called with a context bounded by the drain timeout
        (30s) after the server stopped serving. May be sync or async."""
        self._shutdowns.append(fn)

    @property
    def served(self) -> list[Served]:
        return list(self._served.values())

    @property
    def shutdowns(self) -> list[Callable[[Context], Any]]:
        return list(self._shutdowns)

    # --- in-flight tracking (graceful drain) --------------------------------

    def _enter(self) -> None:
        self._inflight += 1
        self._idle.clear()

    def _leave(self) -> None:
        self._inflight -= 1
        if self._inflight == 0:
            self._idle.set()

    async def wait_idle(self, timeout: float) -> bool:
        try:
            await asyncio.wait_for(self._idle.wait(), timeout)
            return True
        except TimeoutError:
            return False


def _context_for(stream, path: str) -> Context:
    md = Metadata(list(stream.metadata.items()) if stream.metadata else None)
    # grpclib's public Deadline API; both sides count on time.monotonic().
    deadline = time.monotonic() + stream.deadline.time_remaining() if stream.deadline is not None else None
    return Context(incoming=md, deadline=deadline, method=path)


def _to_status(e: BaseException, path: str, ctx: Context) -> GRPCError:
    if isinstance(e, UserError):
        # Go's forCaller: the status message is "<method>: <dev>".
        method = path.lstrip("/").replace("/", ".")
        method = ".".join(method.split(".")[-2:])
        prefix = f"{method}: "
        msg = e.message or ""
        return GRPCError(e.status, msg if msg.startswith(prefix) else prefix + msg, e.details)
    if isinstance(e, GRPCError):
        return e
    if isinstance(e, _RETURNED):
        return GRPCError(Status.UNKNOWN, str(e))
    observability.report_error(ctx, RuntimeError(f"PANIC {path}: {e!r}"))
    log.exception("handler %s raised", path, exc_info=e)
    return GRPCError(Status.INTERNAL, f"{path}: internal error")


ANSWERED_HEADER = "w17-answered"
"""The response header a served method's own ``UNAVAILABLE`` goes out with
(Go: ``grpcerr.AnsweredHeader``)."""


async def _mark_answered(stream, st: GRPCError) -> None:
    """Send response headers carrying :data:`ANSWERED_HEADER` before an
    ``UNAVAILABLE`` the method answered, so the status leaves in the trailers.

    A w17 client replays a trailers-only ``UNAVAILABLE`` — the shape of a
    transport failure that reached no handler. A method that ran may have done
    work first (a write committed, an upstream called); answered this way, the
    client returns the status to its caller instead of running it again."""
    if st.status != Status.UNAVAILABLE:
        return
    try:
        await stream.send_initial_metadata(metadata=[(ANSWERED_HEADER, "1")])
    except ProtocolError:
        pass  # headers already went out: the status is in the trailers anyway


def _span(ctx: Context, path: str):
    tr = observability.tracer()
    if tr is None:
        return None
    from opentelemetry.trace import SpanKind

    return tr.start_as_current_span(path, context=observability.extract_trace(ctx), kind=SpanKind.SERVER,
                                    attributes={"rpc.system": "grpc", "rpc.method": path})


def _handler(m: _Method, served: Served) -> grpclib.const.Handler:
    info, reg = m.info, m.registry
    path = info.path

    async def guarded(stream, body: Callable[[Context], Any]) -> None:
        if reg.draining:
            # Shutting down: refuse new work with the one code a client retries
            # elsewhere, rather than starting what the drain may cut off.
            raise GRPCError(Status.UNAVAILABLE, "server is shutting down")
        _grpclib.bound_received(stream)
        ctx = _context_for(stream, path)
        reg._enter()
        span = _span(ctx, path)
        try:
            if span is None:
                await body(ctx)
            else:
                with span as s:
                    try:
                        await body(ctx)
                    except BaseException as e:
                        from opentelemetry.trace import Status as OStatus, StatusCode

                        s.set_status(OStatus(StatusCode.ERROR, str(e)))
                        raise
        except (asyncio.CancelledError, TimeoutError):
            ctx.cancel()
            raise
        except Exception as e:  # noqa: BLE001
            st = _to_status(e, path, ctx)
            await _mark_answered(stream, st)
            raise st from e
        finally:
            reg._leave()

    if not info.client_streaming and not info.server_streaming:

        async def unary(stream) -> None:
            async def body(ctx: Context) -> None:
                req = await stream.recv_message()
                ctx, headers = paging.with_response_headers(ctx)
                resp = await m.invoke_unary(ctx, req)
                if headers:
                    # The page position a forwarded storage call reported
                    # (paging.relay_position) answers as this method's own.
                    await stream.send_initial_metadata(metadata=[(k, v) for k, vs in headers.items() for v in vs])
                await stream.send_message(resp)

            await guarded(stream, body)

        return grpclib.const.Handler(unary, Cardinality.UNARY_UNARY, info.request, info.response)

    if info.server_streaming and not info.client_streaming:

        async def unary_stream(stream) -> None:
            async def body(ctx: Context) -> None:
                req = await stream.recv_message()
                async for resp in m.fn(ctx, req):
                    await stream.send_message(resp)

            await guarded(stream, body)

        return grpclib.const.Handler(unary_stream, Cardinality.UNARY_STREAM, info.request, info.response)

    async def requests(stream) -> AsyncIterator[Any]:
        async for r in stream:
            yield r

    if info.client_streaming and not info.server_streaming:

        async def stream_unary(stream) -> None:
            async def body(ctx: Context) -> None:
                resp = await m.fn(ctx, requests(stream))
                if resp is None:
                    raise GRPCError(Status.INTERNAL, f"{path}: handler returned no response")
                await stream.send_message(resp)

            await guarded(stream, body)

        return grpclib.const.Handler(stream_unary, Cardinality.STREAM_UNARY, info.request, info.response)

    async def stream_stream(stream) -> None:
        async def body(ctx: Context) -> None:
            ctx = distx._mark_bidi(ctx)
            async for resp in m.fn(ctx, requests(stream)):
                await stream.send_message(resp)

        await guarded(stream, body)

    return grpclib.const.Handler(stream_stream, Cardinality.STREAM_STREAM, info.request, info.response)
