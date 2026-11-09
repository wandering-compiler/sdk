"""The business tier's manifest (``w17-business.json``), formats 1 and 2.

Codegen writes it beside ``descriptors.binpb`` in
``w17/services/<domain>-business/``; the SDK reads it at boot. The platform's
spec ``bundles/business-manifest`` is the contract; this module is its reader:

- ``format`` is checked first, and a format this SDK does not know is refused
  naming the ones it reads;
- format 2 is format 1 plus the ``companion`` section (egress clients and file
  storage served by the business companion); a manifest whose ``companion`` is
  null stays format 1, and a format-1 manifest carrying one is refused;
- a field is never repurposed, so unknown keys are ignored (kept in ``extra``);
- ``null`` (or an empty list) means "this domain has none".

Every value is what the Go business bundle of the same domain uses: the same
variables, the same methods, the same storage services, the same events.
"""

from __future__ import annotations

import json
import os
from collections.abc import Iterable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

__all__ = [
    "SUPPORTED_FORMAT",
    "SUPPORTED_FORMATS",
    "FILES_SERVICE",
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
    "EgressClient",
    "FileConnection",
    "FilePlacement",
    "FileStorage",
    "Companion",
    "Manifest",
    "load",
    "from_dict",
]

SUPPORTED_FORMATS = (1, 2)
SUPPORTED_FORMAT = SUPPORTED_FORMATS[-1]  # the highest format this SDK reads

# The only file service a manifest may name (bizcompanion.FilesService).
FILES_SERVICE = "w17.common.files.W17Files"


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
class EgressClient:
    """One egress client the companion serves (``proto/clients/<client>/``)."""

    client: str  # "quotes"
    service: str  # "clients.quotes.QuotesApi", in the descriptor set
    dsn_env: str  # "W17_CLIENT_QUOTES_DSN" (read by the companion, not here)


@dataclass(frozen=True)
class FileConnection:
    """One upload connection an ``UPLOADED_*`` field of the domain names."""

    name: str
    root_env: str  # read by the companion, not here
    default_root: str = ""
    writable: bool = False  # the business tier writes it (upload.writer: BUSINESS)


@dataclass(frozen=True)
class FilePlacement:
    """One ``writer: BUSINESS`` table field: where its files go."""

    field: str  # full proto name, "app.example.NoteExport.file"
    name: str  # "NoteExportFile", the Go identifier stem
    connection: str
    bucket_path: str = ""
    expected_objects: int = 0
    max_per_bucket: int = 0
    max_size_bytes: int = 0  # 0 = no cap


@dataclass(frozen=True)
class FileStorage:
    service: str
    connections: tuple[FileConnection, ...] = ()
    placements: tuple[FilePlacement, ...] = ()


@dataclass(frozen=True)
class Companion:
    """The business companion: the Go process that serves this tier the
    project's egress clients and the domain's file stores over gRPC."""

    address_env: str
    default_address: str
    egress: tuple[EgressClient, ...] = ()
    files: FileStorage | None = None


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
    companion: Companion | None = None
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
    "plugin_hooks", "companion",
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


def _formats() -> str:
    return " and ".join(str(f) for f in SUPPORTED_FORMATS)


def _unique(values: Iterable[str], what: str) -> None:
    seen: set[str] = set()
    for v in values:
        if v in seen:
            raise ManifestError(f"manifest: {what} {v} is listed twice")
        seen.add(v)


# --- the companion section ------------------------------------------------------
#
# The Go companion (bizcompanion.Load) decodes this section into typed structs
# and refuses what does not fit them; these readers refuse the same, so the
# companion and the tier agree on which manifests boot: a string field takes a
# string, an integer one an integer in its Go type's range (uint32, uint64), a
# bool true or false, a list a list, an object an object; null is the field's
# zero value (a null list element an empty object). A key is matched exactly,
# and one Go's encoding/json would match case-insensitively is refused (the Go
# companion refuses it too); any other unknown key is ignored.

_U32 = (1 << 32) - 1
_U64 = (1 << 64) - 1

# Unicode simple case folding onto the ASCII lowercase field names: A-Z, and
# U+017F (long s) and U+212A (Kelvin), the two non-ASCII runes that fold to an
# ASCII letter.
_FOLD = {**{c: c + 32 for c in range(ord("A"), ord("Z") + 1)}, 0x017F: ord("s"), 0x212A: ord("k")}

_COMPANION_KEYS = frozenset(("address_env", "default_address", "egress", "files"))
_EGRESS_KEYS = frozenset(("client", "service", "dsn_env"))
_FILES_KEYS = frozenset(("service", "connections", "placements"))
_CONNECTION_KEYS = frozenset(("name", "root_env", "default_root", "writable"))
_PLACEMENT_KEYS = frozenset((
    "field", "name", "connection", "bucket_path", "expected_objects", "max_per_bucket", "max_size_bytes"))


class _MinusZero(int):
    """The JSON literal ``-0``: the integer 0 everywhere but an unsigned
    companion field, which refuses it as Go's encoding/json does."""


def _parse_int(s: str) -> int:
    return _MinusZero(0) if s == "-0" else int(s)


def _show(v: Any) -> str:
    return "-0" if type(v) is _MinusZero else json.dumps(v, default=repr)


def _keys(d: dict, names: frozenset[str], where: str) -> None:
    for k in d:
        if not isinstance(k, str) or k in names:
            continue
        folded = k.translate(_FOLD)
        if folded in names:
            raise ManifestError(f"manifest: {where}: key {k!r} is not {folded!r} — keys are matched exactly")


def _str(d: dict, key: str, where: str) -> str:
    """A JSON string; absent (or null) is ""."""
    v = d.get(key)
    if v is None:
        return ""
    if not isinstance(v, str):
        raise ManifestError(f"manifest: {where}{key} must be a string, not {_show(v)}")
    return v


def _req_str(d: dict, key: str, where: str) -> str:
    v = _str(d, key, where)
    if not v:
        raise ManifestError(f"manifest: {where}{key} is required")
    return v


def _bool(d: dict, key: str, where: str) -> bool:
    """A JSON bool; absent (or null) is false. "false" is not a bool."""
    v = d.get(key)
    if v is None:
        return False
    if not isinstance(v, bool):
        raise ManifestError(f"manifest: {where}{key} must be true or false, not {_show(v)}")
    return v


def _uint(d: dict, key: str, where: str, top: int) -> int:
    """A JSON integer from 0 to top; absent (or null) is 0. 1.0, "1" and -0 are not."""
    v = d.get(key)
    if v is None:
        return 0
    if type(v) is not int or not 0 <= v <= top:
        raise ManifestError(f"manifest: {where}{key} must be an integer from 0 to {top}, not {_show(v)}")
    return v


def _objects(d: dict, key: str, where: str) -> list[dict]:
    """A JSON list of objects; absent (or null) is empty, a null element is {}."""
    v = d.get(key)
    if v is None:
        return []
    if not isinstance(v, list):
        raise ManifestError(f"manifest: {where}{key} must be a list or null, not {_show(v)}")
    out = []
    for x in v:
        if x is None:
            x = {}
        elif not isinstance(x, dict):
            raise ManifestError(f"manifest: {where}{key}[] must be an object, not {_show(x)}")
        out.append(x)
    return out


# Then what bizcompanion.Companion.check refuses: the required fields, a name
# listed twice, a placement on an unknown connection, a file service other
# than FILES_SERVICE.
def _companion(d: dict) -> Companion | None:
    c = _obj(d, "companion", "")
    if c is None:
        return None
    _keys(c, _COMPANION_KEYS, "companion")
    address_env = _req_str(c, "address_env", "companion.")
    default_address = _str(c, "default_address", "companion.")
    egress = []
    for e in _objects(c, "egress", "companion."):
        _keys(e, _EGRESS_KEYS, "companion.egress[]")
        egress.append(EgressClient(
            client=_req_str(e, "client", "companion.egress[]."),
            service=_req_str(e, "service", "companion.egress[]."),
            dsn_env=_req_str(e, "dsn_env", "companion.egress[]."),
        ))
    _unique((e.client for e in egress), "companion.egress: client")
    _unique((e.service for e in egress), "companion.egress: service")
    f = _obj(c, "files", "companion.")
    files = None
    if f is not None:
        _keys(f, _FILES_KEYS, "companion.files")
        service = _str(f, "service", "companion.files.")
        if service != FILES_SERVICE:
            raise ManifestError(
                f"manifest: companion.files.service is {service!r}; the companion serves {FILES_SERVICE}")
        conns = []
        for x in _objects(f, "connections", "companion.files."):
            _keys(x, _CONNECTION_KEYS, "companion.files.connections[]")
            conns.append(FileConnection(
                name=_req_str(x, "name", "companion.files.connections[]."),
                root_env=_req_str(x, "root_env", "companion.files.connections[]."),
                default_root=_str(x, "default_root", "companion.files.connections[]."),
                writable=_bool(x, "writable", "companion.files.connections[]."),
            ))
        names = [x.name for x in conns]
        _unique(names, "companion.files.connections:")
        placements = []
        for p in _objects(f, "placements", "companion.files."):
            _keys(p, _PLACEMENT_KEYS, "companion.files.placements[]")
            w = "companion.files.placements[]."
            placements.append(FilePlacement(
                field=_req_str(p, "field", w),
                name=_str(p, "name", w),
                connection=_req_str(p, "connection", w),
                bucket_path=_str(p, "bucket_path", w),
                expected_objects=_uint(p, "expected_objects", w, _U64),
                max_per_bucket=_uint(p, "max_per_bucket", w, _U32),
                max_size_bytes=_uint(p, "max_size_bytes", w, _U64),
            ))
        _unique((p.field for p in placements), "companion.files.placements:")
        for p in placements:
            if p.connection not in names:
                raise ManifestError(
                    f"manifest: companion.files.placements: {p.field} is on connection {p.connection!r}, "
                    "which is not in companion.files.connections")
        files = FileStorage(service=service, connections=tuple(conns), placements=tuple(placements))
    return Companion(
        address_env=address_env,
        default_address=default_address,
        egress=tuple(egress),
        files=files,
    )


def from_dict(d: dict, base_dir: str = ".") -> Manifest:
    fmt = d.get("format")
    if not isinstance(fmt, int) or isinstance(fmt, bool) or fmt < 1:
        raise ManifestError(f"manifest: format {fmt!r} is not a manifest format version (this SDK reads {_formats()})")
    if fmt not in SUPPORTED_FORMATS:
        raise ManifestError(
            f"manifest: format {fmt} is not one this SDK reads (it reads formats {_formats()}) — "
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
    companion = _companion(d)
    if companion is not None and fmt < 2:
        raise ManifestError(f"manifest: format {fmt} has no companion section; a manifest with one is format 2")
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
        companion=companion,
        base_dir=base_dir,
        extra={k: v for k, v in d.items() if k not in _TOP},
    )


def load(path: str | Path) -> Manifest:
    p = Path(path)
    try:
        d = json.loads(p.read_text(encoding="utf-8"), parse_int=_parse_int)
    except OSError as e:
        raise ManifestError(f"manifest: read {p}: {e}") from e
    except ValueError as e:  # JSONDecodeError, or an integer past int's digit limit
        raise ManifestError(f"manifest: {p} is not JSON: {e}") from e
    if not isinstance(d, dict):
        raise ManifestError(f"manifest: {p} is not a JSON object")
    return from_dict(d, base_dir=str(p.parent))
