"""The business tier's manifest (``w17-business.json``), format 1.

Codegen writes it beside ``descriptors.binpb`` in
``w17/services/<domain>-business/``; the SDK reads it at boot. The platform's
spec ``bundles/business-manifest`` is the contract; this module is its reader:

- ``format`` is checked first, and a format this SDK does not know is refused
  naming the one it reads;
- a field is never repurposed, so unknown keys are ignored (kept in ``extra``);
- ``null`` (or an empty list) means "this domain has none".

Every value is what the Go business bundle of the same domain uses: the same
variables, the same methods, the same storage services, the same events.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

__all__ = [
    "SUPPORTED_FORMAT",
    "ManifestError",
    "Register",
    "MethodSpec",
    "ServiceSpec",
    "ConfigKey",
    "StorageTier",
    "PayloadBinding",
    "Emit",
    "Invalidate",
    "Events",
    "PluginHook",
    "Manifest",
    "load",
    "from_dict",
]

SUPPORTED_FORMAT = 1


class ManifestError(Exception):
    pass


@dataclass(frozen=True)
class Register:
    """Where the author's code is: ``path`` is project-relative; the SDK puts
    its parent on the import path and imports ``module`` (``<package>.w17``)."""

    path: str
    package: str
    module: str
    function: str = "register_business"


@dataclass(frozen=True)
class MethodSpec:
    name: str
    path: str
    input: str = ""
    output: str = ""
    client_streaming: bool = False
    server_streaming: bool = False


@dataclass(frozen=True)
class ServiceSpec:
    name: str
    methods: tuple[MethodSpec, ...] = ()


@dataclass(frozen=True)
class ConfigKey:
    name: str
    env: str
    type: str = "string"  # string | bool | int
    default: str = ""  # normalised to the text a variable would hold
    required: bool = False
    secret: bool = False
    description: str = ""


@dataclass(frozen=True)
class StorageTier:
    address_env: str
    default_address: str
    services: tuple[str, ...] = ()
    # The coordinator's full service name, "" when the storage bundle
    # registers none (no mutation service).
    distributed_transactions: str = ""


@dataclass(frozen=True)
class PayloadBinding:
    """One event field and where its value comes from."""

    field: str
    source: str = "literal"  # request | response | literal
    path: tuple[str, ...] = ()
    value: str = ""  # for literal


@dataclass(frozen=True)
class Emit:
    """A method's ``(w17.event_emit)``."""

    method: str  # the gRPC path, "/pkg.Svc/Method"
    event: str  # the event message's full name
    envelope: str  # the envelope message's full name (events.envelope)
    envelope_field: str  # the envelope's payload case for this event
    channel: str
    topic: str
    fields: tuple[PayloadBinding, ...] = ()


@dataclass(frozen=True)
class Invalidate:
    """A method's ``(w17.touches_entities)``: one ``w17.InvalidateEvent`` per
    entity, published after the method's emits."""

    method: str
    channel: str
    topic: str
    group: str
    entities: tuple[str, ...] = ()


@dataclass(frozen=True)
class Events:
    nats_dsn_env: str = ""  # "" when the project runs no NATS
    redis_dsn_env: str = ""  # "" when no channel is on Redis Streams
    redis_channels: tuple[str, ...] = ()
    durable_prefix_env: str = ""
    durable_prefix_default: str = ""
    envelope: str = ""
    emits: tuple[Emit, ...] = ()
    invalidates: tuple[Invalidate, ...] = ()


@dataclass(frozen=True)
class PluginHook:
    plugin: str
    registered_as: str
    slot: str
    methods: tuple[str, ...] = ()  # gRPC paths


@dataclass(frozen=True)
class Manifest:
    format: int
    domain: str
    bundle: str
    language: str
    sdk_version: str
    env_prefix: str
    listen_env: str
    default_listen_addr: str
    reflection_env: str
    shutdown_timeout: float
    descriptor_set: str  # relative to the manifest
    register: Register
    services: tuple[ServiceSpec, ...] = ()
    config: tuple[ConfigKey, ...] = ()
    storage: StorageTier | None = None
    events: Events | None = None
    plugin_hooks: tuple[PluginHook, ...] = ()
    base_dir: str = "."
    extra: dict[str, Any] = field(default_factory=dict, compare=False, hash=False)

    def path(self, rel: str) -> str:
        return rel if os.path.isabs(rel) else os.path.normpath(os.path.join(self.base_dir, rel))

    @property
    def emits(self) -> tuple[Emit, ...]:
        return self.events.emits if self.events else ()

    @property
    def invalidates(self) -> tuple[Invalidate, ...]:
        return self.events.invalidates if self.events else ()


_TOP = (
    "format", "domain", "bundle", "language", "sdk_version", "env_prefix", "listen", "reflection_env",
    "shutdown_timeout_seconds", "descriptor_set", "register", "services", "config", "storage", "events",
    "plugin_hooks",
)


def _req(d: dict, key: str, where: str) -> Any:
    v = d.get(key)
    if v is None or v == "":
        raise ManifestError(f"manifest: {where}{key} is required")
    return v


def _obj(d: dict, key: str, where: str) -> dict | None:
    v = d.get(key)
    if v is None:
        return None
    if not isinstance(v, dict):
        raise ManifestError(f"manifest: {where}{key} must be an object or null")
    return v


def _default_text(typ: str, v: Any) -> str:
    # The manifest types a default as its key's type (25, true); config reads
    # it as the text an env variable would hold, through the same parser.
    if isinstance(v, bool):
        return "true" if v else "false"
    if v is None:
        return ""
    return str(v)


def from_dict(d: dict, base_dir: str = ".") -> Manifest:
    fmt = d.get("format")
    if not isinstance(fmt, int) or isinstance(fmt, bool) or fmt < 1:
        raise ManifestError(f"manifest: format {fmt!r} is not a manifest format version (this SDK reads {SUPPORTED_FORMAT})")
    if fmt != SUPPORTED_FORMAT:
        raise ManifestError(
            f"manifest: format {fmt} is not one this SDK reads (it reads format {SUPPORTED_FORMAT}) — "
            "install the w17-business release of the project's sdk_version")
    language = d.get("language", "python")
    if language != "python":
        raise ManifestError(f"manifest: language {language!r} is not python — this is the Python business SDK")
    domain = _req(d, "domain", "")
    prefix = d.get("env_prefix") or domain.upper().replace("-", "_")
    listen = _obj(d, "listen", "") or {}
    reg = _obj(d, "register", "")
    if reg is None:
        raise ManifestError("manifest: register is required")
    path = _req(reg, "path", "register.")
    package = reg.get("package") or Path(path).name
    register = Register(
        path=path,
        package=package,
        module=reg.get("module") or f"{package}.w17",
        function=reg.get("function") or "register_business",
    )
    services = tuple(
        ServiceSpec(
            name=_req(s, "name", "services[]."),
            methods=tuple(
                MethodSpec(
                    name=_req(m, "name", "services[].methods[]."),
                    path=m.get("path") or f"/{s['name']}/{m['name']}",
                    input=m.get("input", ""),
                    output=m.get("output", ""),
                    client_streaming=bool(m.get("client_streaming", False)),
                    server_streaming=bool(m.get("server_streaming", False)),
                )
                for m in s.get("methods") or ()
            ),
        )
        for s in d.get("services") or ()
    )
    config = tuple(
        ConfigKey(
            name=_req(c, "name", "config[]."),
            env=c.get("env") or f"{prefix}_{c['name'].upper()}",
            type=c.get("type") or "string",
            default=_default_text(c.get("type") or "string", c.get("default")),
            required=bool(c.get("required", False)),
            secret=bool(c.get("secret", False)),
            description=c.get("description", ""),
        )
        for c in d.get("config") or ()
    )
    st = _obj(d, "storage", "")
    storage = None
    if st is not None:
        dt = st.get("distributed_transactions")
        storage = StorageTier(
            address_env=_req(st, "address_env", "storage."),
            default_address=st.get("default_address", ""),
            services=tuple(st.get("services") or ()),
            distributed_transactions=(dt or {}).get("service", "") if isinstance(dt, dict) else "",
        )
    ev = _obj(d, "events", "")
    events = None
    if ev is not None:
        envelope = ev.get("envelope", "")
        nats = ev.get("nats") or {}
        redis = ev.get("redis_streams") or {}
        emits = tuple(
            Emit(
                method=_req(e, "method", "events.emits[]."),
                event=_req(e, "event", "events.emits[]."),
                envelope=e.get("envelope") or envelope,
                envelope_field=_req(e, "envelope_field", "events.emits[]."),
                channel=_req(e, "channel", "events.emits[]."),
                topic=_req(e, "topic", "events.emits[]."),
                fields=tuple(
                    PayloadBinding(
                        field=_req(f, "field", "events.emits[].fields[]."),
                        source=f.get("source") or "literal",
                        path=tuple(f.get("path") or ()),
                        value=f.get("value", ""),
                    )
                    for f in e.get("fields") or ()
                ),
            )
            for e in ev.get("emits") or ()
        )
        for e in emits:
            if not e.envelope:
                raise ManifestError(f"manifest: events.envelope is required for the emit of {e.method}")
        invalidates = tuple(
            Invalidate(
                method=_req(i, "method", "events.invalidates[]."),
                channel=_req(i, "channel", "events.invalidates[]."),
                topic=i.get("topic") or "w17.invalidate",
                group=i.get("group", ""),
                entities=tuple(i.get("entities") or ()),
            )
            for i in ev.get("invalidates") or ()
        )
        events = Events(
            nats_dsn_env=nats.get("dsn_env", ""),
            redis_dsn_env=redis.get("dsn_env", ""),
            redis_channels=tuple(redis.get("channels") or ()),
            durable_prefix_env=ev.get("durable_prefix_env", ""),
            durable_prefix_default=ev.get("durable_prefix_default", ""),
            envelope=envelope,
            emits=emits,
            invalidates=invalidates,
        )
    hooks = tuple(
        PluginHook(
            plugin=h.get("plugin", ""),
            registered_as=h.get("registered_as", ""),
            slot=h.get("slot", ""),
            methods=tuple(h.get("methods") or ()),
        )
        for h in d.get("plugin_hooks") or ()
    )
    return Manifest(
        format=fmt,
        domain=domain,
        bundle=d.get("bundle") or f"{domain}-business",
        language=language,
        sdk_version=d.get("sdk_version", ""),
        env_prefix=prefix,
        listen_env=listen.get("env") or f"{prefix}_LISTEN_ADDR",
        default_listen_addr=listen.get("default") or ":50051",
        reflection_env=d.get("reflection_env") or f"{prefix}_GRPC_REFLECTION",
        shutdown_timeout=float(d.get("shutdown_timeout_seconds") or 10),
        descriptor_set=d.get("descriptor_set") or "descriptors.binpb",
        register=register,
        services=services,
        config=config,
        storage=storage,
        events=events,
        plugin_hooks=hooks,
        base_dir=base_dir,
        extra={k: v for k, v in d.items() if k not in _TOP},
    )


def load(path: str | Path) -> Manifest:
    p = Path(path)
    try:
        d = json.loads(p.read_text(encoding="utf-8"))
    except OSError as e:
        raise ManifestError(f"manifest: read {p}: {e}") from e
    except json.JSONDecodeError as e:
        raise ManifestError(f"manifest: {p} is not JSON: {e}") from e
    if not isinstance(d, dict):
        raise ManifestError(f"manifest: {p} is not a JSON object")
    return from_dict(d, base_dir=str(p.parent))
