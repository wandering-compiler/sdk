"""The business tier's lifecycle: boot from the manifest, serve, drain.

``python -m w17_business serve --manifest w17-business.json --project-root DIR``
does what Go's generated business ``main`` does:

1. read the manifest, load ``descriptor_set`` into the pool;
2. the config (a value that does not parse, or a required key with no value,
   stops the boot naming it);
3. Sentry and OpenTelemetry from ``W17_SENTRY_DSN`` / ``W17_OTEL_*``;
4. the buses: NATS and/or Redis Streams, each DSN resolved through the
   secrets chain; a bus whose DSN is empty is off and its events are dropped,
   as Go leaves the dispatcher nil;
5. the storage clients (one connection, dialled lazily) and, when the
   manifest has a ``companion`` section, the companion's: its egress clients
   and file stores (a second connection, also lazy);
6. import ``register.module`` from ``--project-root`` and call
   ``register_business(cfg, registry, clients)`` (sync or async); every method
   a plugin hook is bound to must then have a handler;
7. serve h2c, or TLS with ``W17_INTERNAL_TLS``; ``grpc.health.v1``; reflection
   when ``<PREFIX>_GRPC_REFLECTION`` is ``1``/``true``;
8. on SIGTERM/SIGINT: report NOT_SERVING, refuse new calls, give the
   in-flight ones ``shutdown_timeout_seconds`` (10, Go's ShutdownTimeout),
   cancel the rest and give them one more timeout to return (a rollback, a
   commit in progress), wait for the work committed transactions still owe
   (events, on_commit hooks), then run the shutdown hooks under one 30s
   drain deadline (Go's shutdownDrainTimeout), close the buses and the
   channels, flush telemetry. The whole drain is ONE budget, set when it
   starts: every step draws from what is left of it.
"""

from __future__ import annotations

import asyncio
import importlib
import inspect
import logging
import os
import signal
import sys
from collections.abc import Mapping
from typing import Any

from grpclib.config import Configuration
from grpclib.health.check import ServiceStatus
from grpclib.health.service import OVERALL, Health
from grpclib.server import Server

from . import _grpclib, events, observability, principal, tlsconfig, txscope
from .client import CompanionConn, Conn, ServiceClient, TxCoordinator, parse_addr
from .config import Config, ConfigError
from .context import Context
from .descriptors import Pool, default_pool
from .errors import StatusDetailsCodec
from .files import FileStore
from .manifest import Manifest, load
from .secrets import new_resolver
from .server import Registry, RegistrationError

__all__ = ["Clients", "App", "BootError", "SHUTDOWN_TIMEOUT", "DRAIN_TIMEOUT", "serve"]

log = logging.getLogger("w17_business")

SHUTDOWN_TIMEOUT = 10.0  # lifecycle.DefaultShutdownTimeout; the manifest's shutdown_timeout_seconds wins
# runtime.shutdownDrainTimeout: the budget of the whole drain — the server's
# stop, the handlers and the work after a commit, the shutdown hooks and the
# bus close — unless the shutdown timeout is longer.
DRAIN_TIMEOUT = 30.0
ENV_MANIFEST = "W17_BUSINESS_MANIFEST"
ENV_PROJECT_ROOT = "W17_BUSINESS_PROJECT_ROOT"


class BootError(Exception):
    """The tier cannot start; the message names what to fix."""


class Clients:
    """The tier's clients, built from the manifest's ``storage`` and
    ``companion`` sections.

    ``clients.service("shop.core.OrderQuery")`` (or the short name when
    unambiguous, also as an attribute: ``clients.OrderQuery``) returns a
    ``ServiceClient``; ``clients.distributed_transaction()`` returns the
    coordinator ``distx`` drives. One connection serves them all.

    ``clients.egress("quotes")`` (the client name, or its service's full name)
    is an egress client and ``clients.store("app-exports")`` a connection's
    ``FileStore``: both are served by the business companion, over a second
    connection of their own (see ``client.CompanionConn``)."""

    def __init__(self, pool: Pool, manifest: Manifest, environ: Mapping[str, str] | None = None) -> None:
        env = os.environ if environ is None else environ
        self._pool = pool
        self._conn: Conn | None = None
        self._companion: CompanionConn | None = None
        self._by_service: dict[str, ServiceClient] = {}
        self._egress: dict[str, ServiceClient] = {}  # by client name
        self._stores: dict[str, FileStore] = {}
        self._tx: TxCoordinator | None = None
        self._storage(pool, manifest, env)
        self._companion_clients(pool, manifest, env)

    def _storage(self, pool: Pool, manifest: Manifest, env: Mapping[str, str]) -> None:
        tier = manifest.storage
        if tier is None:
            return
        addr = env.get(tier.address_env) or tier.default_address
        if not addr:
            raise BootError(f"storage: {tier.address_env} is not set and the manifest gives no default address")
        try:
            parse_addr(addr)
        except ValueError as e:
            raise BootError(f"storage: {tier.address_env}={addr!r}: {e}") from e
        try:
            conn = Conn(addr, environ=dict(env))
        except ValueError as e:
            raise BootError(f"storage tls: {e}") from e
        self._conn = conn
        for svc in tier.services:
            try:
                full = pool.service(svc).full_name
            except KeyError as e:
                raise BootError(f"storage: service {svc!r} is not in the descriptor set") from e
            self._by_service[full] = ServiceClient(conn, full, pool)
        if tier.distributed_transactions:
            self._tx = TxCoordinator(conn)

    def _companion_clients(self, pool: Pool, manifest: Manifest, env: Mapping[str, str]) -> None:
        comp = manifest.companion
        if comp is None:
            return
        addr = env.get(comp.address_env) or comp.default_address
        if not addr:
            raise BootError(f"companion: {comp.address_env} is not set and the manifest gives no default address")
        try:
            parse_addr(addr)
        except ValueError as e:
            raise BootError(f"companion: {comp.address_env}={addr!r}: {e}") from e
        try:
            conn = CompanionConn(addr, environ=dict(env))
        except ValueError as e:
            raise BootError(f"companion tls: {e}") from e
        self._companion = conn
        for e in comp.egress:
            try:
                full = pool.service(e.service).full_name
            except KeyError as err:
                raise BootError(f"companion: egress client {e.client}: service {e.service!r} is not in the descriptor set") from err
            self._egress[e.client] = ServiceClient(conn, full, pool)
        files = comp.files
        if files is None:
            return
        try:
            pool.service(files.service)
        except KeyError as err:
            raise BootError(f"companion: files: service {files.service!r} is not in the descriptor set") from err
        for c in files.connections:
            self._stores[c.name] = FileStore(conn, c, files.placements, pool)

    def egress(self, name: str) -> ServiceClient:
        """The egress client ``name`` (``"quotes"``) or the one serving the
        service ``name`` (``"clients.quotes.QuotesApi"``)."""
        n = name.lstrip(".")
        if n in self._egress:
            return self._egress[n]
        for c in self._egress.values():
            if c.service == n:
                return c
        raise KeyError(f"clients: no egress client {name!r} (have: {sorted(self._egress)})")

    def store(self, connection: str) -> FileStore:
        """The file store of the upload connection ``connection``."""
        s = self._stores.get(connection)
        if s is None:
            raise KeyError(f"clients: no upload connection {connection!r} (have: {sorted(self._stores)})")
        return s

    def service(self, name: str) -> ServiceClient:
        name = name.lstrip(".")
        if name in self._by_service:
            return self._by_service[name]
        matches = [s for s in self._by_service if s.rsplit(".", 1)[-1] == name]
        if len(matches) == 1:
            return self._by_service[matches[0]]
        if not matches:
            raise KeyError(f"clients: no storage service {name!r} (have: {sorted(self._by_service)})")
        raise KeyError(f"clients: {name!r} is ambiguous ({', '.join(sorted(matches))})")

    def __getattr__(self, name: str) -> ServiceClient:
        if name.startswith("_"):
            raise AttributeError(name)
        try:
            return self.service(name)
        except KeyError as e:
            raise AttributeError(str(e)) from None

    def distributed_transaction(self) -> TxCoordinator:
        if self._tx is None:
            raise RuntimeError(
                "clients: this domain's storage registers no distributed-transaction coordinator "
                "(it declares no mutation service), so there is no transaction to begin")
        return self._tx

    def close(self) -> None:
        if self._conn is not None:
            self._conn.close()
        if self._companion is not None:
            self._companion.close()


class App:
    """One business tier, booted from a manifest. ``run`` serves it."""

    def __init__(self, manifest: Manifest, environ: Mapping[str, str] | None = None,
                 project_root: str | None = None, drain_timeout: float = DRAIN_TIMEOUT) -> None:
        self.manifest = manifest
        # The drain's budget (DRAIN_TIMEOUT); tests shorten it.
        self.drain_timeout = drain_timeout
        self.env = dict(os.environ if environ is None else environ)
        self.project_root = project_root or self.env.get(ENV_PROJECT_ROOT) or os.getcwd()
        self.pool = default_pool()
        self.obs: observability.Observability | None = None
        self.cfg: Config | None = None
        self.clients: Clients | None = None
        self.registry: Registry | None = None
        self.dispatcher: events.Dispatcher | None = None
        self.server: Server | None = None
        self.health_status = ServiceStatus()
        self.addr = ""
        self.port = 0

    # --- boot ----------------------------------------------------------------

    async def boot(self) -> None:
        m = self.manifest
        try:
            self.pool.add_set_file(m.path(m.descriptor_set))
        except OSError as e:
            raise BootError(f"descriptor_set: {m.path(m.descriptor_set)}: {e}") from e
        self._auth_resp()
        try:
            self.cfg = Config.load(m.config, self.env)
        except ConfigError as e:
            raise BootError(str(e)) from e
        try:
            self.obs = observability.init(m.bundle, scrub_fields=self.cfg.secret_env_names(), environ=self.env)
        except ValueError as e:
            raise BootError(f"observability: {e}") from e
        self.dispatcher = await self._bus()
        self.clients = Clients(self.pool, m, self.env)
        self.registry = Registry(
            self.pool, m.emits, self.dispatcher, m.invalidates, services=tuple(s.name for s in m.services))
        self._check_events()
        await self._register()

    def _auth_resp(self) -> None:
        # The project's AuthResp, for principal.user_id's envelope fallback:
        # the one message of that name the descriptor set carries.
        found = self.pool.find_messages_named("AuthResp")
        if len(found) == 1:
            principal.set_auth_resp_type(self.pool.message(found[0]))

    def _check_events(self) -> None:
        for e in self.manifest.emits:
            try:
                events.EnvelopeBuilder(self.pool, e)
            except (KeyError, ValueError) as err:
                raise BootError(f"events: {e.method}: {err}") from err

    async def _bus(self) -> events.Dispatcher | None:
        ev = self.manifest.events
        if ev is None or not (ev.nats_dsn_env or ev.redis_dsn_env):
            return None
        try:
            res = new_resolver(self.env)
        except Exception as e:
            raise BootError(f"secrets: {e}") from e
        nats = redis = None
        try:
            if ev.nats_dsn_env and (dsn := res.get(ev.nats_dsn_env)):
                nats = await events.NatsPublisher.connect(dsn, domain=self.manifest.domain)
        except Exception as e:
            raise BootError(f"eventbus nats: {ev.nats_dsn_env}: {e}") from e
        try:
            if ev.redis_dsn_env and (dsn := res.get(ev.redis_dsn_env)):
                redis = await events.RedisPublisher.connect(dsn, domain=self.manifest.domain)
        except Exception as e:
            raise BootError(f"eventbus redis: {ev.redis_dsn_env}: {e}") from e
        if nats is None and redis is None:
            return None
        return events.Router(nats, redis, ev.redis_channels)

    async def _register(self) -> None:
        m = self.manifest
        reg = m.register
        pkg_dir = os.path.normpath(os.path.join(self.project_root, reg.path))
        parent = os.path.dirname(pkg_dir)
        if parent not in sys.path:
            sys.path.insert(0, parent)
        try:
            mod = importlib.import_module(reg.module)
        except ImportError as e:
            raise BootError(f"register: import {reg.module} (from {pkg_dir}): {e}") from e
        fn = getattr(mod, reg.function, None)
        if fn is None or not callable(fn):
            raise BootError(f"register: {reg.module} has no {reg.function}(cfg, registry, clients)")
        try:
            r = fn(self.cfg, self.registry, self.clients)
            if inspect.isawaitable(r):
                await r
        except RegistrationError as e:
            raise BootError(str(e)) from e
        served_paths = {p for s in self.registry.served for p in s.__mapping__()}
        for hook in m.plugin_hooks:
            for path in hook.methods:
                if path not in served_paths:
                    raise BootError(
                        f"plugin hook {hook.plugin}/{hook.slot} ({hook.registered_as}) calls {path}, which has no "
                        "handler: register its service, or the plugin's call fails at runtime")
        served = {s.service for s in self.registry.served}
        for svc in m.services:
            if svc.name not in served:
                log.info("%s is not registered; its calls answer Unimplemented, as in Go", svc.name)

    # --- serve ---------------------------------------------------------------

    def listen_addr(self) -> str:
        return self.env.get(self.manifest.listen_env) or self.manifest.default_listen_addr

    async def start(self) -> None:
        reg = self.registry
        assert reg is not None
        self.health_status.set(True)
        handlers: list[Any] = list(reg.served)
        handlers.append(Health({OVERALL: [self.health_status]}))
        if self.env.get(self.manifest.reflection_env, "") in ("1", "true"):
            from grpclib.reflection.service import ServerReflection

            handlers = ServerReflection.extend(handlers)
        try:
            ssl_ctx = tlsconfig.server_context(self.env)
        except ValueError as e:
            raise BootError(f"server tls: {e}") from e
        self.server = Server(
            handlers,
            status_details_codec=StatusDetailsCodec(),
            config=Configuration(
                # runtime.serverOptions: ping an idle client every 30s, 10s to answer.
                _keepalive_time=30.0,
                _keepalive_timeout=10.0,
                _keepalive_permit_without_calls=True,
                _http2_max_pings_without_data=0,
            ),
        )
        listen = self.listen_addr()
        try:
            host, port = parse_addr(listen)
        except ValueError as e:
            raise BootError(f"{self.manifest.listen_env}={listen!r}: {e}") from e
        if listen.startswith(":"):
            host = None  # ":50051" listens on every interface, as in Go
        await self.server.start(host, port, ssl=ssl_ctx)
        try:
            aserver = _grpclib.listener(self.server)
        except _grpclib.GrpclibAPIError as e:
            raise BootError(str(e)) from e
        if aserver is None:
            raise BootError("grpclib: the server started but keeps no listener (Server._server is None)")
        sock = aserver.sockets[0] if aserver.sockets else None
        self.port = sock.getsockname()[1] if sock else port
        self.addr = f"{host or '0.0.0.0'}:{self.port}"
        log.info("%s listening on %s%s", self.manifest.bundle, self.addr, " (TLS)" if ssl_ctx else "")

    async def shutdown(self) -> None:
        reg, srv = self.registry, self.server
        timeout = self.manifest.shutdown_timeout or SHUTDOWN_TIMEOUT
        log.info("%s: shutting down", self.manifest.bundle)
        # One budget for the whole drain, set once, now: Go's shutdown drain
        # timeout, or the operator's shutdown timeout when that is longer.
        # Every step below draws from what is left of it, so a deployment's
        # termination grace bounds the drain as a whole, not each step alone.
        drain = Context().with_timeout(max(self.drain_timeout, timeout))

        def left() -> float:
            return max(drain.time_remaining() or 0.0, 0.0)

        self.health_status.set(False)
        if reg is not None:
            reg.draining = True
        aserver = None
        if srv is not None:
            # Go's GracefulStop bounded by the timeout, then Stop. grpclib has
            # no graceful stop: stop accepting connections, give the in-flight
            # calls the timeout, then let grpclib cancel the rest.
            aserver = _grpclib.listener(srv)
            if aserver is not None:
                aserver.close()
            stop = min(timeout, left())
            if reg is not None and not await reg.wait_idle(stop):
                observability.report_error(None, f"runtime: gRPC handler(s) still running after {stop:g}s; cancelling them")
            srv.close()
            # Since Python 3.12 asyncio's wait_closed waits for every client
            # connection to close, and a peer tier keeps its connection open
            # (keepalive) indefinitely: close them, or the drain never ends.
            if aserver is not None and hasattr(aserver, "close_clients"):
                aserver.close_clients()
        # After the server's close, one more timeout (within the budget),
        # computed once and SHARED by the two waits below:
        #  - the handlers it cancelled: the server's close does not wait for
        #    them, and one still running (a rollback, a commit) needs the
        #    storage channel and the bus it would close under it;
        #  - the work a committed transaction (or an emit) owes, which
        #    outlives its request. Waited for only after the handlers: one
        #    cancelled may have committed and started that work just then.
        # What is still running at the deadline is cancelled (bounded by the
        # rest of the budget) and reported, and only then does the bus close.
        loop = asyncio.get_running_loop()
        after_stop = loop.time() + min(timeout, left())

        def until_after_stop() -> float:
            return max(after_stop - loop.time(), 0.0)

        if srv is not None:
            try:
                await asyncio.wait_for(srv.wait_closed(), max(until_after_stop(), 0.001))
            except TimeoutError:
                if aserver is not None and hasattr(aserver, "abort_clients"):
                    aserver.abort_clients()
        if not await txscope.drain(until_after_stop()):
            n = await txscope.cancel_pending(max(left(), 0.001))
            observability.report_error(
                None, f"runtime: {n} piece(s) of work after a commit (events, on_commit hooks) still running "
                      "at the shutdown deadline; cancelled")
        # The shutdown hooks and the bus close get the rest of the budget.
        for i, fn in enumerate(reg.shutdowns if reg else []):
            try:
                r = fn(drain)
                if inspect.isawaitable(r):
                    await asyncio.wait_for(r, max(left(), 0.001))
            except Exception as e:  # noqa: BLE001
                observability.report_error(drain, RuntimeError(f"runtime: shutdown hook {i}: {e!r}"))
        if self.dispatcher is not None:
            try:
                await asyncio.wait_for(self.dispatcher.close(), max(left(), 0.001))
            except Exception as e:  # noqa: BLE001
                log.warning("eventbus close: %s", e)
        if self.clients is not None:
            self.clients.close()
        if self.obs is not None:
            self.obs.shutdown()

    async def run(self, stop: asyncio.Event | None = None) -> None:
        stop = stop or asyncio.Event()
        await self.boot()
        await self.start()
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            try:
                loop.add_signal_handler(sig, stop.set)
            except (NotImplementedError, RuntimeError):
                pass
        try:
            await stop.wait()
        finally:
            await self.shutdown()


def manifest_path(arg: str | None, environ: Mapping[str, str] | None = None) -> str:
    env = os.environ if environ is None else environ
    return arg or env.get(ENV_MANIFEST, "") or "w17-business.json"


def serve(manifest: str | None = None, project_root: str | None = None) -> None:
    """Boot and serve until SIGTERM/SIGINT."""
    logging.basicConfig(level=os.environ.get("W17_LOG_LEVEL", "INFO"),
                        format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    m = load(manifest_path(manifest))
    asyncio.run(App(m, project_root=project_root).run())
