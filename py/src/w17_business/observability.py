"""Sentry and OpenTelemetry, configured as Go's ``core/observx`` configures them.

- ``W17_SENTRY_DSN`` turns Sentry on (traces sampled at 0.2, server name = the
  service, release = ``W17_SERVICE_VERSION``, environment = ``W17_ENV``).
- ``W17_OTEL_ENDPOINT`` (fallback ``OTEL_EXPORTER_OTLP_ENDPOINT``) turns OTLP/gRPC
  trace export on; ``OTEL_SERVICE_NAME`` overrides the service name.
- ``W17_OTEL_TLS`` — on|true|1|yes / off|false|0|no; empty follows the internal
  mesh (``W17_INTERNAL_TLS``), with its CA and client pair.
- W3C trace context (``traceparent``/``tracestate``) and baggage are the
  propagators, installed only when OTel export is on — as in Go, where the
  global propagator stays the no-op one otherwise.

``report_error`` is ``observx.ReportError``: Sentry when on, the log always.
"""

from __future__ import annotations

import logging
import os
import re
from collections.abc import Iterable
from typing import Any

from . import tlsconfig
from .context import Context

__all__ = [
    "Observability",
    "init",
    "report_error",
    "extract_trace",
    "inject_trace",
    "scrub_event",
]

log = logging.getLogger("w17_business")

_state: dict[str, Any] = {"sentry": False, "otel": False, "tracer": None, "provider": None}

# Request headers that carry a credential by definition, and the name segments
# that flag one — the same lists lib/eventbus strips from an event's metadata,
# applied here to what Sentry would otherwise keep from a failing request.
_SCRUB_PLACEHOLDER = "[scrubbed]"


def report_error(ctx: Context | None, err: BaseException | str) -> None:
    """Report an error an operator must see, without failing anything.

    Mirrors observx.ReportError: to Sentry when it is on, and to the log
    always (Go falls through to log.Printf without Sentry)."""
    text = err if isinstance(err, str) else f"{type(err).__name__}: {err}"
    log.error("error: %s", text)
    if _state["sentry"]:
        import sentry_sdk

        if isinstance(err, BaseException):
            sentry_sdk.capture_exception(err)
        else:
            sentry_sdk.capture_message(err, level="error")


def _truthy(v: str) -> bool:
    return v.strip().lower() in ("on", "true", "1", "yes")


def _falsy(v: str) -> bool:
    return v.strip().lower() in ("off", "false", "0", "no")


class Observability:
    """What ``init`` turned on; ``shutdown`` flushes and stops it."""

    def __init__(self, sentry: bool, otel: bool) -> None:
        self.sentry = sentry
        self.otel = otel

    def shutdown(self, timeout: float = 5.0) -> None:
        if self.sentry:
            import sentry_sdk

            sentry_sdk.flush(timeout=timeout)
        provider = _state.get("provider")
        if provider is not None:
            try:
                provider.shutdown()
            except Exception as e:  # noqa: BLE001
                log.warning("otel shutdown: %s", e)
            _state["provider"] = None
        _state["sentry"] = _state["otel"] = False
        _state["tracer"] = None


def scrub_event(event: dict, scrub_fields: Iterable[str] = ()) -> dict:
    """Redact credentials and the given field names from a Sentry event.

    The request headers a credential rides in are dropped whatever the
    project declares; ``scrub_fields`` (the secret config keys, by default)
    are redacted wherever they appear as a key in request data or frame
    locals, or as ``name=value`` inside a string (a repr of a message)."""
    from .events import is_credential_key

    fields = frozenset(scrub_fields)
    patterns = [
        re.compile(rf"(?i)['\"]?\b{re.escape(f)}\b['\"]?\s*[=:]\s*('[^']*'|\"[^\"]*\"|[^\s,)\]}}]+)")
        for f in fields
    ]

    def scrub(v: Any) -> Any:
        if isinstance(v, dict):
            for k in list(v):
                if k in fields or (isinstance(k, str) and is_credential_key(k)):
                    v[k] = _SCRUB_PLACEHOLDER
                else:
                    v[k] = scrub(v[k])
            return v
        if isinstance(v, list):
            return [scrub(x) for x in v]
        if isinstance(v, str):
            for p in patterns:
                v = p.sub(lambda m: m.group(0).replace(m.group(1), _SCRUB_PLACEHOLDER), v)
            return v
        return v

    req = event.get("request")
    if isinstance(req, dict):
        for key in ("headers", "data", "cookies"):
            if key in req:
                req[key] = scrub(req[key])
    for container in ("exception", "threads"):
        values = (event.get(container) or {}).get("values") or []
        for value in values:
            frames = ((value or {}).get("stacktrace") or {}).get("frames") or []
            for fr in frames:
                if isinstance(fr.get("vars"), dict):
                    fr["vars"] = scrub(fr["vars"])
    return event


def init(service_name: str, *, scrub_fields: Iterable[str] = (), environ: dict[str, str] | None = None) -> Observability:
    """Turn Sentry and OpenTelemetry on per the environment.

    A bad value (an unknown ``W17_OTEL_TLS``) raises: as in Go, the only valid
    recovery from a bad telemetry config is to fix it and restart."""
    env = os.environ if environ is None else environ
    service_name = env.get("OTEL_SERVICE_NAME") or service_name
    version = env.get("W17_SERVICE_VERSION", "")
    environment = env.get("W17_ENV", "")
    sentry_on = False
    otel_on = False

    dsn = env.get("W17_SENTRY_DSN", "")
    if dsn:
        import sentry_sdk

        fields = tuple(scrub_fields)
        sentry_sdk.init(
            dsn=dsn,
            server_name=service_name,
            release=version or None,
            environment=environment or None,
            traces_sample_rate=0.2,
            # Go's Sentry client sends no request bodies or IPs by default;
            # keep the Python one equally quiet.
            send_default_pii=False,
            before_send=lambda event, hint: scrub_event(event, fields),
        )
        sentry_on = True

    endpoint = env.get("W17_OTEL_ENDPOINT") or env.get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
    if endpoint:
        otel_tls = env.get("W17_OTEL_TLS", "")
        if otel_tls and not (_truthy(otel_tls) or _falsy(otel_tls)):
            raise ValueError(f"W17_OTEL_TLS={otel_tls!r} is not a recognised value (on|true|1|yes, off|false|0|no)")
        mesh = env.get(tlsconfig.ENV_INTERNAL_TLS, "")
        tlsconfig.check(mesh)
        secure = _truthy(otel_tls) if otel_tls else tlsconfig.dial_tls(mesh)
        if otel_tls and _falsy(otel_tls) and tlsconfig.dial_tls(mesh):
            log.warning("observability: W17_OTEL_TLS=%s exports telemetry in plaintext while %s is on", otel_tls, tlsconfig.ENV_INTERNAL_TLS)

        from opentelemetry import propagate, trace
        from opentelemetry.baggage.propagation import W3CBaggagePropagator
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
        from opentelemetry.propagators.composite import CompositePropagator
        from opentelemetry.sdk.resources import Resource
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
        from opentelemetry.sdk.trace.sampling import ParentBased, TraceIdRatioBased
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

        creds = None
        if secure:
            creds = tlsconfig.otlp_credentials(env)
        exporter = OTLPSpanExporter(endpoint=endpoint, insecure=not secure, credentials=creds)
        resource = Resource.create(
            {
                "service.name": service_name,
                "service.version": version,
                "deployment.environment.name": environment,
            }
        )
        provider = TracerProvider(resource=resource, sampler=ParentBased(TraceIdRatioBased(1.0)))
        provider.add_span_processor(BatchSpanProcessor(exporter))
        trace.set_tracer_provider(provider)
        propagate.set_global_textmap(
            CompositePropagator([TraceContextTextMapPropagator(), W3CBaggagePropagator()])
        )
        _state["provider"] = provider
        _state["tracer"] = trace.get_tracer("w17_business")
        otel_on = True

    _state["sentry"], _state["otel"] = sentry_on, otel_on
    return Observability(sentry_on, otel_on)


def tracer():
    """The SDK's tracer, or None when OTel export is off."""
    return _state["tracer"]


def extract_trace(ctx: Context):
    """The OTel context the incoming metadata names (traceparent), or None."""
    if not _state["otel"]:
        return None
    from opentelemetry import propagate

    carrier = {k: v[0] for k, v in ctx.incoming.items() if v}
    return propagate.extract(carrier)


def inject_trace(pairs: list[tuple[str, str]]) -> list[tuple[str, str]]:
    """Add the current span's trace context to outgoing metadata pairs."""
    if not _state["otel"]:
        return pairs
    from opentelemetry import propagate

    carrier: dict[str, str] = {}
    propagate.inject(carrier)
    if not carrier:
        return pairs
    keep = [(k, v) for k, v in pairs if k not in carrier]
    return keep + list(carrier.items())
