"""Booting the conformance tier from its manifest: config failures, health,
reflection, the drain."""

import asyncio

import pytest
from grpclib.client import Channel
from grpclib.health.v1.health_grpc import HealthStub
from grpclib.health.v1.health_pb2 import HealthCheckRequest, HealthCheckResponse

from w17_business import observability, tlsconfig
from w17_business.__main__ import _probe_target, main, parse_duration, probe
from w17_business.manifest import load
from w17_business.runtime import App, BootError

from conftest import MANIFEST

PROJECT = str(MANIFEST.parent)


def env(**kw):
    base = {"NOTES_API_TOKEN": "t0ken", "NOTES_LISTEN_ADDR": "127.0.0.1:0", "NOTES_STORAGE_BACKEND_ADDR": "127.0.0.1:1"}
    base.update(kw)
    return {k: v for k, v in base.items() if v is not None}


async def test_missing_required_key_stops_the_boot(chdir_tmp):
    app = App(load(MANIFEST), project_root=PROJECT, environ=env(NOTES_API_TOKEN=None))
    with pytest.raises(BootError, match="'api_token'.*NOTES_API_TOKEN is required"):
        await app.boot()


async def test_bad_value_stops_the_boot(chdir_tmp):
    app = App(load(MANIFEST), project_root=PROJECT, environ=env(NOTES_LIMIT="many"))
    with pytest.raises(BootError, match="NOTES_LIMIT"):
        await app.boot()


async def test_bad_internal_tls_stops_the_boot(chdir_tmp):
    app = App(load(MANIFEST), project_root=PROJECT, environ=env(W17_INTERNAL_TLS="maybe"))
    await app.boot()
    with pytest.raises(BootError, match="W17_INTERNAL_TLS"):
        await app.start()


async def test_serves_health_and_drains(chdir_tmp):
    app = App(load(MANIFEST), project_root=PROJECT, environ=env(NOTES_GRPC_REFLECTION="1"))
    await app.boot()
    await app.start()
    hook = []
    app.registry.register_shutdown(lambda ctx: hook.append(ctx.time_remaining()))
    ch = Channel("127.0.0.1", app.port)
    try:
        resp = await HealthStub(ch).Check(HealthCheckRequest())
        assert resp.status == HealthCheckResponse.SERVING
        await probe(f"127.0.0.1:{app.port}", {}, 2.0)
    finally:
        ch.close()
    await app.shutdown()
    assert hook and 0 < hook[0] <= 30, "the hook gets the drain deadline"


async def test_the_drain_waits_for_in_flight_calls(chdir_tmp, msg):
    app = App(load(MANIFEST), project_root=PROJECT, environ=env())
    await app.boot()
    await app.start()
    from w17_business.client import Conn, ServiceClient
    from w17_business.context import Context

    conn = Conn(f"127.0.0.1:{app.port}", tls=False)
    client = ServiceClient(conn, "conformance.v1.NoteService", app.pool)
    call = asyncio.ensure_future(client.Sleep(Context(), msg("SleepRequest")(ms=300)))
    await asyncio.sleep(0.1)
    await app.shutdown()
    assert (await call).text == "slept", "an in-flight call finishes during the drain"
    conn.close()


def test_probe_target_matches_go():
    assert _probe_target(":50061") == ("127.0.0.1", 50061)
    assert _probe_target("0.0.0.0:1") == ("127.0.0.1", 1)
    assert _probe_target("[::]:2") == ("127.0.0.1", 2)
    with pytest.raises(ValueError):
        _probe_target(":0")


def test_parse_duration():
    assert parse_duration("3s") == 3 and parse_duration("500ms") == 0.5 and parse_duration("1m") == 60


def test_health_command_fails_when_nothing_listens(monkeypatch, capsys):
    monkeypatch.setenv("NOTES_LISTEN_ADDR", "127.0.0.1:1")
    assert main(["health", "--manifest", str(MANIFEST), "--timeout", "1s"]) == 1
    assert "FAIL grpc 127.0.0.1:1" in capsys.readouterr().out


def test_serve_command_reports_a_boot_error(monkeypatch, capsys, chdir_tmp):
    monkeypatch.delenv("NOTES_API_TOKEN", raising=False)
    monkeypatch.setenv("NOTES_LISTEN_ADDR", "127.0.0.1:0")
    assert main(["serve", "--manifest", str(MANIFEST), "--project-root", PROJECT]) == 1
    assert "NOTES_API_TOKEN is required" in capsys.readouterr().err


def test_internal_tls_values():
    for v in ("", "off", "0", "no", "false"):
        tlsconfig.check(v)
        assert tlsconfig.server_context({tlsconfig.ENV_INTERNAL_TLS: v}) is None
        assert tlsconfig.client_context({tlsconfig.ENV_INTERNAL_TLS: v}) is None
    assert tlsconfig.server_context({tlsconfig.ENV_INTERNAL_TLS: "terminated"}) is None
    assert tlsconfig.client_context({tlsconfig.ENV_INTERNAL_TLS: "terminated"}) is not None
    with pytest.raises(ValueError, match="not both set"):
        tlsconfig.server_context({tlsconfig.ENV_INTERNAL_TLS: "on"})
    # An unrecognised value dials TLS rather than plaintext.
    assert tlsconfig.client_context({tlsconfig.ENV_INTERNAL_TLS: "maybe"}) is not None


def test_sentry_scrub_redacts_credentials_and_secret_fields():
    event = {
        "request": {"headers": {"Authorization": "Bearer x", "x-api-key": "k", "accept": "*/*"},
                    "data": {"NOTES_API_TOKEN": "t0ken", "q": "token NOTES_API_TOKEN=t0ken here"}},
        "exception": {"values": [{"stacktrace": {"frames": [{"vars": {"NOTES_API_TOKEN": "t0ken", "x": 1}}]}}]},
    }
    out = observability.scrub_event(event, ["NOTES_API_TOKEN"])
    h = out["request"]["headers"]
    assert h["Authorization"] == h["x-api-key"] == "[scrubbed]" and h["accept"] == "*/*"
    assert "t0ken" not in str(out)


def test_otel_init_refuses_a_bad_tls_value():
    with pytest.raises(ValueError, match="W17_OTEL_TLS"):
        observability.init("svc", environ={"W17_OTEL_ENDPOINT": "otel:4317", "W17_OTEL_TLS": "maybe"})


def test_otel_propagates_traceparent_when_on():
    obs = observability.init("svc", environ={"W17_OTEL_ENDPOINT": "127.0.0.1:1"})
    try:
        from opentelemetry import trace

        from w17_business.context import Context

        tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
        parent = observability.extract_trace(Context(incoming={"traceparent": tp}))
        with observability.tracer().start_as_current_span("x", context=parent):
            pairs = observability.inject_trace([("a", "b")])
        d = dict(pairs)
        assert d["traceparent"].split("-")[1] == "0af7651916cd43dd8448eb211c80319c"
        assert trace.get_current_span() is not None
    finally:
        obs.shutdown(timeout=0.1)
    assert observability.inject_trace([("a", "b")]) == [("a", "b")], "off again: nothing injected"


async def test_a_plugin_hook_without_a_handler_stops_the_boot(chdir_tmp):
    import dataclasses

    from w17_business.manifest import PluginHook

    m = load(MANIFEST)
    m = dataclasses.replace(m, plugin_hooks=(PluginHook("auth", "auth", "after_signup", ("/conformance.v1.NoteService/Nope",)),))
    with pytest.raises(BootError, match="after_signup.*Nope.*no handler"):
        await App(m, project_root=PROJECT, environ=env()).boot()


async def test_registering_a_service_the_manifest_does_not_list_is_refused(chdir_tmp):
    import dataclasses

    m = dataclasses.replace(load(MANIFEST), services=())
    with pytest.raises(BootError, match="not a business service of this tier"):
        await App(m, project_root=PROJECT, environ=env()).boot()


async def test_a_missing_register_module_names_it(chdir_tmp, tmp_path):
    import dataclasses

    from w17_business.manifest import Register

    m = dataclasses.replace(load(MANIFEST), register=Register("nowhere/pkg_x", "pkg_x", "pkg_x.w17"))
    with pytest.raises(BootError, match="import pkg_x.w17"):
        await App(m, project_root=str(tmp_path), environ=env()).boot()


# --- internal TLS: only the configured trust, and no silent half-config -------

from pathlib import Path  # noqa: E402
import ssl  # noqa: E402

TLS = Path(__file__).parent / "testdata" / "tls"
CA, LEAF, KEY = str(TLS / "ca.pem"), str(TLS / "leaf.pem"), str(TLS / "leaf.key")
ON = {tlsconfig.ENV_INTERNAL_TLS: "on"}


@pytest.fixture
def default_trust(monkeypatch):
    """Records every time a context reaches for the system's trust store."""
    calls = []
    real_load, real_paths = ssl.SSLContext.load_default_certs, ssl.SSLContext.set_default_verify_paths

    def load(self, *a, **kw):
        calls.append("load_default_certs")
        return real_load(self, *a, **kw)

    def paths(self, *a, **kw):
        calls.append("set_default_verify_paths")
        return real_paths(self, *a, **kw)

    monkeypatch.setattr(ssl.SSLContext, "load_default_certs", load)
    monkeypatch.setattr(ssl.SSLContext, "set_default_verify_paths", paths)
    return calls


def test_client_trusts_only_the_configured_ca(default_trust):
    ctx = tlsconfig.client_context({**ON, tlsconfig.ENV_INTERNAL_TLS_CA: CA})
    assert default_trust == [], "the system roots were trusted beside W17_INTERNAL_TLS_CA"
    assert [c["subject"] for c in ctx.get_ca_certs()] == [((("commonName", "w17 test CA"),),)]
    assert ctx.verify_mode == ssl.CERT_REQUIRED and ctx.check_hostname


def test_client_without_a_ca_uses_the_system_roots_like_go(default_trust):
    # Go leaves RootCAs nil (the system pool) when no CA is configured, and
    # "terminated" always verifies the platform's certificate against it.
    assert tlsconfig.client_context(ON) is not None and default_trust
    default_trust.clear()
    assert tlsconfig.client_context({tlsconfig.ENV_INTERNAL_TLS: "terminated", tlsconfig.ENV_INTERNAL_TLS_CA: CA})
    assert default_trust


def test_server_verifies_clients_against_the_client_ca_only(default_trust):
    ctx = tlsconfig.server_context({**ON, tlsconfig.ENV_INTERNAL_TLS_CERT: LEAF, tlsconfig.ENV_INTERNAL_TLS_KEY: KEY,
                                    tlsconfig.ENV_INTERNAL_TLS_CLIENT_CA: CA})
    assert default_trust == [], "client certificates were accepted from the system roots too"
    assert ctx.verify_mode == ssl.CERT_REQUIRED and len(ctx.get_ca_certs()) == 1
    plain = tlsconfig.server_context({**ON, tlsconfig.ENV_INTERNAL_TLS_CERT: LEAF, tlsconfig.ENV_INTERNAL_TLS_KEY: KEY})
    assert plain.verify_mode == ssl.CERT_NONE and plain.get_ca_certs() == []


@pytest.mark.parametrize("extra,match", [
    ({tlsconfig.ENV_INTERNAL_TLS_CA: "/nonexistent/ca.pem"}, "W17_INTERNAL_TLS_CA"),
    ({tlsconfig.ENV_INTERNAL_TLS_CA: KEY}, "W17_INTERNAL_TLS_CA"),
    ({tlsconfig.ENV_INTERNAL_TLS_CERT: LEAF}, "not both set"),
    ({tlsconfig.ENV_INTERNAL_TLS_KEY: KEY}, "not both set"),
    ({tlsconfig.ENV_INTERNAL_TLS_CERT: LEAF, tlsconfig.ENV_INTERNAL_TLS_KEY: "/nonexistent/key"}, "W17_INTERNAL_TLS_CERT"),
    ({tlsconfig.ENV_INTERNAL_TLS_CERT: CA, tlsconfig.ENV_INTERNAL_TLS_KEY: KEY}, "W17_INTERNAL_TLS_CERT"),
])
def test_client_refuses_a_broken_tls_config(extra, match):
    with pytest.raises(ValueError, match=match):
        tlsconfig.client_context({**ON, **extra})


def test_server_refuses_an_unreadable_client_ca():
    for bad in ("/nonexistent/ca.pem", KEY):
        with pytest.raises(ValueError, match="W17_INTERNAL_TLS_CLIENT_CA"):
            tlsconfig.server_context({**ON, tlsconfig.ENV_INTERNAL_TLS_CERT: LEAF, tlsconfig.ENV_INTERNAL_TLS_KEY: KEY,
                                      tlsconfig.ENV_INTERNAL_TLS_CLIENT_CA: bad})


async def test_a_broken_client_tls_config_stops_the_boot(chdir_tmp):
    app = App(load(MANIFEST), project_root=PROJECT, environ=env(W17_INTERNAL_TLS="on", W17_INTERNAL_TLS_CA="/nonexistent"))
    with pytest.raises(BootError, match="W17_INTERNAL_TLS_CA"):
        await app.boot()


async def test_mutual_tls_round_trip():
    # The server presents the leaf and requires a client cert from the CA; the
    # client verifies the leaf against that CA alone and presents the same leaf.
    srv_ctx = tlsconfig.server_context({**ON, tlsconfig.ENV_INTERNAL_TLS_CERT: LEAF, tlsconfig.ENV_INTERNAL_TLS_KEY: KEY,
                                        tlsconfig.ENV_INTERNAL_TLS_CLIENT_CA: CA})

    async def handle(reader, writer):
        writer.write(b"ok")
        await writer.drain()
        writer.close()

    server = await asyncio.start_server(handle, "127.0.0.1", 0, ssl=srv_ctx)
    port = server.sockets[0].getsockname()[1]
    try:
        cli = tlsconfig.client_context({**ON, tlsconfig.ENV_INTERNAL_TLS_CA: CA, tlsconfig.ENV_INTERNAL_TLS_CERT: LEAF,
                                        tlsconfig.ENV_INTERNAL_TLS_KEY: KEY})
        r, w = await asyncio.open_connection("127.0.0.1", port, ssl=cli, server_hostname="biz.test")
        assert await r.read() == b"ok"
        w.close()
        # Without the client pair the server refuses the handshake.
        bare = tlsconfig.client_context({**ON, tlsconfig.ENV_INTERNAL_TLS_CA: CA})
        try:
            r, w = await asyncio.open_connection("127.0.0.1", port, ssl=bare, server_hostname="biz.test")
            got = await r.read()  # TLS 1.3: the refusal arrives after the client's handshake
        except (ssl.SSLError, ConnectionError):
            got = None
        assert got != b"ok"
    finally:
        server.close()
        await server.wait_closed()


def test_health_probe_names_the_leaf_with_public_api():
    from w17_business.__main__ import _leaf_server_name

    assert _leaf_server_name(LEAF) == "biz.test"
    assert _leaf_server_name(CA) is None
    assert _leaf_server_name("/nonexistent") is None


# --- Sentry: no frame locals, and the error text itself is scrubbed -----------


def test_sentry_sends_no_local_variables(monkeypatch):
    import sentry_sdk

    got = {}
    monkeypatch.setattr(sentry_sdk, "init", lambda **kw: got.update(kw))
    observability.init("svc", environ={"W17_SENTRY_DSN": "https://k@sentry.invalid/1"})
    observability._state["sentry"] = False
    assert got.get("include_local_variables") is False


def test_sentry_scrub_reaches_exception_values_and_messages():
    event = {
        "exception": {"values": [{"type": "RuntimeError", "value": "dial failed: NOTES_API_TOKEN=t0ken, retry"}]},
        "message": "boot: NOTES_API_TOKEN: 't0ken'",
        "logentry": {"message": "with NOTES_API_TOKEN=%s", "formatted": "with NOTES_API_TOKEN=t0ken",
                     "params": ["NOTES_API_TOKEN=t0ken"]},
    }
    out = observability.scrub_event(event, ["NOTES_API_TOKEN"])
    assert "t0ken" not in str(out), out
    assert out["exception"]["values"][0]["value"] == "dial failed: NOTES_API_TOKEN=[scrubbed], retry"


# --- grpclib's private API: pinned, and checked at boot -----------------------


def test_grpclib_is_pinned_exactly():
    import re as _re

    pyproject = (Path(__file__).resolve().parents[1] / "pyproject.toml").read_text()
    assert _re.search(r'"grpclib==\d+\.\d+\.\d+"', pyproject), "grpclib must be pinned to one version"


async def test_a_grpclib_without_the_private_listener_stops_the_boot(chdir_tmp, monkeypatch):
    from w17_business import runtime

    class Renamed(runtime.Server):
        async def start(self, *a, **kw):
            await super().start(*a, **kw)
            self._listener = self.__dict__.pop("_server")

    monkeypatch.setattr(runtime, "Server", Renamed)
    app = App(load(MANIFEST), project_root=PROJECT, environ=env())
    await app.boot()
    with pytest.raises(BootError, match="grpclib"):
        await app.start()
    app.server._server = app.server._listener
    app.server.close()
    await app.server.wait_closed()


async def test_shutdown_drains_work_started_by_closing_the_server_before_closing_the_bus(chdir_tmp):
    # grpclib's close cancels the handlers still running; one cancelled after
    # its commit starts its events then. The drain must come after that, and
    # the bus must close only once they are done.
    from w17_business import txscope

    app = App(load(MANIFEST), project_root=PROJECT, environ=env())
    await app.boot()
    await app.start()
    done, at_close = [], {}

    async def emit():
        await asyncio.sleep(0.1)
        done.append(1)

    real_close = app.server.close

    def close():
        asyncio.ensure_future(txscope.shielded(emit()))
        real_close()

    app.server.close = close

    class Bus:
        async def close(self):
            at_close["done"] = list(done)

    app.dispatcher = Bus()
    await app.shutdown()
    assert at_close == {"done": [1]}, "the bus closed before the work the server's close started"
