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
