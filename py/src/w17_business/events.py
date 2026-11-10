"""Events: the envelope, its context data, and the brokers — byte-compatible
with sdk/go/lib/eventbus and the generated Go emit helpers.

The envelope is the domain's events-lock message (``<pkg>.Event``): the event in
its ``payload`` oneof, ``metadata`` (50001) with the originating request's
metadata, ``labels`` (50002) with its broadcast labels. A Go subscriber decodes
what this module publishes exactly as it decodes a Go emit.

Brokers, as the Go adapters address them:

- NATS JetStream: stream ``<channel>`` over subjects ``<channel>.>`` (limits
  retention, discard old, max age 7 days, no count limit — created or updated
  on first use, as Go's ``ensureStream``), subject ``<channel>.<topic>``,
  payload the marshalled envelope, header ``W17-Domain: <domain>``;
- Redis Streams: ``XADD <channel> MINID ~ <now-7d>-0 * topic <topic> payload
  <envelope bytes> domain <domain>``.

The domain stamp is the manifest's domain (eventbus ``Domain`` option): two
domains that declare one channel name share its stream on one broker, and a
Go subscriber with a Domain settles a message stamped with another domain
unread. A publisher without a domain writes no stamp.

Emits are fire-and-forget: a failure is logged and reported, never raised into
the RPC, and bounded by the publish timeout (2s).
"""

from __future__ import annotations

import asyncio
import logging
import time
import unicodedata
from collections.abc import Callable
from typing import Any, Protocol

from google.protobuf.descriptor import FieldDescriptor
from google.protobuf.message import Message

from . import principal
from .context import Context, Metadata
from .descriptors import Pool
from .manifest import Emit, Invalidate
from .observability import report_error

__all__ = [
    "Dispatcher",
    "NatsPublisher",
    "RedisPublisher",
    "MemoryPublisher",
    "metadata_from_context",
    "labels_from_context",
    "reserved_metadata_key",
    "is_credential_key",
    "validate_channel",
    "validate_topic",
    "EnvelopeBuilder",
    "Router",
    "invalidate_events",
    "fire",
    "DEFAULT_STREAM_MAX_AGE",
    "DEFAULT_PUBLISH_TIMEOUT",
    "LABEL_KEY_PREFIX",
]

log = logging.getLogger("w17_business.events")

LABEL_KEY_PREFIX = principal.LABEL_KEY_PREFIX
DEFAULT_STREAM_MAX_AGE = 7 * 24 * 3600.0  # eventbus.DefaultStreamMaxAge
DEFAULT_PUBLISH_TIMEOUT = 2.0  # eventbus.DefaultPublishTimeout
MAX_CHANNEL_LEN = 200

# --- names (eventbus/names.go) ---------------------------------------------


class InvalidChannelError(ValueError):
    pass


class InvalidTopicError(ValueError):
    pass


def _name_char(c: str) -> bool:
    return ("a" <= c <= "z") or ("A" <= c <= "Z") or ("0" <= c <= "9") or c in "_-"


def validate_channel(name: str) -> None:
    if not name:
        raise InvalidChannelError("eventbus: invalid channel name: empty")
    if len(name.encode()) > MAX_CHANNEL_LEN:
        raise InvalidChannelError(f"eventbus: invalid channel name: {name!r} is longer than {MAX_CHANNEL_LEN} characters")
    for c in name:
        if not _name_char(c):
            raise InvalidChannelError(
                f"eventbus: invalid channel name: {name!r} contains {c!r} (allowed: letters, digits, '_' and '-')")
    low = name.lower()
    if low == "w17" or low.startswith("w17_"):
        raise InvalidChannelError(f"eventbus: invalid channel name: {name!r} is reserved for the platform")


def validate_topic(topic: str) -> None:
    if not topic:
        raise InvalidTopicError("eventbus: invalid topic: empty")
    for tok in topic.split("."):
        if not tok:
            raise InvalidTopicError(f"eventbus: invalid topic: {topic!r} has an empty segment")
        for c in tok:
            if c in "*>" or c.isspace() or unicodedata.category(c) == "Cc":
                raise InvalidTopicError(f"eventbus: invalid topic: {topic!r} contains {c!r}")


# --- context data (eventbus/eventctx.go) -------------------------------------

_RESERVED = frozenset({"w17-tx-id", "w17-conn-id"})
_CREDENTIAL_HEADERS = frozenset(
    {"authorization", "proxy-authorization", "cookie", "set-cookie", "x-w17-credential"}
)
_CREDENTIAL_SEGMENTS = frozenset(
    {
        "auth", "authorization", "apikey", "bearer", "cookie", "credential", "credentials",
        "jwt", "key", "otp", "pass", "passwd", "password", "pwd", "secret", "session",
        "signature", "token",
    }
)
_GRPC_GATEWAY_PREFIX = "grpcgateway-"


def is_credential_key(k: str) -> bool:
    """Whether a metadata key carries a request credential (eventbus.isCredentialKey)."""
    k = k.strip().lower().removeprefix(_GRPC_GATEWAY_PREFIX)
    if k in _CREDENTIAL_HEADERS:
        return True
    if principal.is_gateway_owned_key(k):
        return False
    segs = [s for s in _split_any(k, "-_.") if s]
    for i, seg in enumerate(segs):
        if seg in _CREDENTIAL_SEGMENTS:
            return True
        if seg == "api" and i + 1 < len(segs) and segs[i + 1] == "key":
            return True
    return False


def _split_any(s: str, seps: str) -> list[str]:
    out, cur = [], []
    for c in s:
        if c in seps:
            out.append("".join(cur))
            cur = []
        else:
            cur.append(c)
    out.append("".join(cur))
    return out


def reserved_metadata_key(k: str) -> bool:
    """Stripped from an envelope: the transaction keys and credentials."""
    return k.lower() in _RESERVED or is_credential_key(k)


def _context_md(ctx: Context) -> Metadata | None:
    # Incoming first, outgoing second, never a merge (eventbus.contextMD).
    if len(ctx.incoming) > 0:
        return ctx.incoming
    if len(ctx.outgoing) > 0:
        return ctx.outgoing
    return None


def metadata_from_context(ctx: Context) -> dict[str, str] | None:
    """eventbus.MetadataFromContext: the request's metadata flattened for
    ``Event.metadata`` — gateway-owned keys keep their FIRST value, every other
    key its LAST non-empty one; transaction keys and credentials are dropped."""
    md = _context_md(ctx)
    if md is None:
        return None
    flat: dict[str, str] = {}
    for k, vs in md.items():
        if not vs or reserved_metadata_key(k):
            continue
        flat[k] = vs[0] if principal.is_gateway_owned_key(k) else _last_non_empty(vs)
    return flat


def _last_non_empty(vs: list[str]) -> str:
    """The last value that is not blank, else the last value."""
    for v in reversed(vs):
        if v.strip():
            return v
    return vs[-1]


def labels_from_context(ctx: Context) -> dict[str, str] | None:
    """eventbus.LabelsFromContext: ``w17-label-*`` with the prefix stripped,
    first value; None when the event is unlabelled."""
    md = _context_md(ctx)
    if md is None:
        return None
    labels: dict[str, str] | None = None
    for k, vs in md.items():
        if not vs or len(k) <= len(LABEL_KEY_PREFIX) or not k.startswith(LABEL_KEY_PREFIX):
            continue
        if labels is None:
            labels = {}
        labels[k[len(LABEL_KEY_PREFIX):]] = vs[0]
    return labels


# --- the envelope ----------------------------------------------------------


class EnvelopeBuilder:
    """Builds one emit's envelope from (request, response), as the generated Go
    ``emit<Svc><Method>`` helper does.

    A payload binding reads a field path through the request or the response
    (Go's chained getters: an unset message on the way reads as its zero
    value) or takes a literal string. A proto3 ``optional`` target gets
    presence even for a zero value, as Go's ``proto.String(x)`` wrap gives it;
    a message target is copied only when the source message is set (Go assigns
    the pointer, nil included)."""

    def __init__(self, pool: Pool, emit: Emit) -> None:
        self.emit = emit
        self.envelope_cls = pool.message(emit.envelope)
        self.event_cls = pool.message(emit.event)
        env_d = self.envelope_cls.DESCRIPTOR
        if emit.envelope_field not in env_d.fields_by_name:
            raise ValueError(f"emit {emit.method}: envelope {emit.envelope} has no payload case {emit.envelope_field!r}")
        for name in ("metadata", "labels"):
            if name not in env_d.fields_by_name:
                raise ValueError(f"emit {emit.method}: envelope {emit.envelope} has no {name} map")
        ev_d = self.event_cls.DESCRIPTOR
        for b in emit.fields:
            if b.field not in ev_d.fields_by_name:
                raise ValueError(f"emit {emit.method}: event {emit.event} has no field {b.field!r}")

    def build(self, ctx: Context, req: Message, resp: Message) -> Message:
        ev = self.event_cls()
        for b in self.emit.fields:
            target = ev.DESCRIPTOR.fields_by_name[b.field]
            if b.source == "literal":
                _assign(ev, target, b.value, present=True)
                continue
            src = req if b.source == "request" else resp
            value, present = _walk(src, b.path)
            _assign(ev, target, value, present=present)
        env = self.envelope_cls()
        md = metadata_from_context(ctx)
        if md:
            env.metadata.update(md)
        labels = labels_from_context(ctx)
        if labels:
            env.labels.update(labels)
        getattr(env, self.emit.envelope_field).CopyFrom(ev)
        return env


def _walk(msg: Message, path: tuple[str, ...]) -> tuple[Any, bool]:
    cur: Any = msg
    present = True
    for i, seg in enumerate(path):
        fd = cur.DESCRIPTOR.fields_by_name.get(seg)
        if fd is None:
            raise ValueError(f"payload path {'.'.join(path)}: {cur.DESCRIPTOR.full_name} has no field {seg!r}")
        is_msg = fd.type == FieldDescriptor.TYPE_MESSAGE and not _is_repeated(fd)
        if is_msg and not cur.HasField(seg):
            present = False
        cur = getattr(cur, seg)
    return cur, present


def _is_repeated(fd: FieldDescriptor) -> bool:
    # protobuf 6+ has is_repeated; older releases only label.
    rep = getattr(fd, "is_repeated", None)
    if rep is not None:
        return bool(rep)
    return fd.label == FieldDescriptor.LABEL_REPEATED


def _assign(ev: Message, fd: FieldDescriptor, value: Any, *, present: bool) -> None:
    if fd.message_type is not None and fd.message_type.GetOptions().map_entry:
        getattr(ev, fd.name).update(value)
        return
    if _is_repeated(fd):
        getattr(ev, fd.name).extend(value)
        return
    if fd.type == FieldDescriptor.TYPE_MESSAGE:
        if present:
            getattr(ev, fd.name).CopyFrom(value)
        return
    setattr(ev, fd.name, value)


# --- brokers ---------------------------------------------------------------


class Dispatcher(Protocol):
    async def dispatch(self, ctx: Context, channel: str, topic: str, envelope: Message) -> None: ...

    async def close(self) -> None: ...


class MemoryPublisher:
    """Records dispatches in memory (tests, and a tier with no broker)."""

    def __init__(self) -> None:
        self.published: list[tuple[str, str, bytes]] = []

    async def dispatch(self, ctx: Context, channel: str, topic: str, envelope: Message) -> None:
        validate_channel(channel)
        validate_topic(topic)
        self.published.append((channel, topic, envelope.SerializeToString()))

    async def close(self) -> None:
        pass


#: eventbus.DomainHeader: the NATS header carrying the emitting domain.
DOMAIN_HEADER = "W17-Domain"
#: The Redis stream entry field carrying it.
REDIS_FIELD_DOMAIN = "domain"


class NatsPublisher:
    """Publishes to NATS JetStream as eventbus.NatsBus.Dispatch does."""

    def __init__(self, nc, js, *, publish_timeout: float = DEFAULT_PUBLISH_TIMEOUT,
                 max_age: float = DEFAULT_STREAM_MAX_AGE, max_msgs: int = -1,
                 domain: str = "") -> None:
        self._nc, self._js = nc, js
        self._headers = {DOMAIN_HEADER: domain} if domain else None
        self._timeout = self.publish_timeout = publish_timeout
        self._max_age, self._max_msgs = max_age, max_msgs
        self._streams: set[str] = set()

    @classmethod
    async def connect(cls, dsn: str, *, connect_wait: float = 30.0, **kw: Any) -> NatsPublisher:
        """Connect, retrying an unreachable broker for connect_wait seconds
        (eventbus.DefaultConnectWait: a broker started in the same deploy)."""
        import nats

        if not (dsn.startswith("nats://") or dsn.startswith("tls://")):
            raise ValueError("NATS DSN must use nats:// or tls:// scheme")
        deadline = time.monotonic() + connect_wait
        while True:
            try:
                nc = await nats.connect(
                    dsn, allow_reconnect=True, max_reconnect_attempts=-1, connect_timeout=2,
                )
                break
            except Exception as e:  # noqa: BLE001
                if time.monotonic() >= deadline:
                    raise ConnectionError(f"nats connect: {e}") from e
                await asyncio.sleep(0.5)
        return cls(nc, nc.jetstream(), **kw)

    async def _ensure_stream(self, channel: str) -> None:
        if channel in self._streams:
            return
        from nats.js.api import DiscardPolicy, RetentionPolicy, StreamConfig
        from nats.js.errors import NotFoundError

        cfg = StreamConfig(
            name=channel,
            subjects=[channel + ".>"],
            retention=RetentionPolicy.LIMITS,
            discard=DiscardPolicy.OLD,
            max_age=self._max_age if self._max_age > 0 else None,
            max_msgs=self._max_msgs,
        )
        # CreateOrUpdateStream: update first, create when it does not exist.
        try:
            await self._js.update_stream(cfg)
        except NotFoundError:
            await self._js.add_stream(cfg)
        self._streams.add(channel)

    async def dispatch(self, ctx: Context, channel: str, topic: str, envelope: Message) -> None:
        validate_channel(channel)
        validate_topic(topic)
        if self._nc.is_closed:
            raise ConnectionError("eventbus nats: connection is closed")
        from nats.js.errors import NoStreamResponseError, NotFoundError

        raw = envelope.SerializeToString()
        subject = f"{channel}.{topic}"

        async def go() -> None:
            await self._ensure_stream(channel)
            try:
                await self._js.publish(subject, raw, timeout=self._timeout, headers=self._headers)
            except (NoStreamResponseError, NotFoundError):
                # The broker lost the stream (recreated without persistence):
                # forget it, re-create it, publish once more.
                self._streams.discard(channel)
                await self._ensure_stream(channel)
                await self._js.publish(subject, raw, timeout=self._timeout, headers=self._headers)

        await asyncio.wait_for(go(), self._timeout)

    async def close(self) -> None:
        if not self._nc.is_closed:
            await self._nc.drain()


class RedisPublisher:
    """Appends to Redis Streams as eventbus.RedisBus.Dispatch does."""

    def __init__(self, client, *, publish_timeout: float = DEFAULT_PUBLISH_TIMEOUT,
                 max_age: float = DEFAULT_STREAM_MAX_AGE, max_msgs: int = 0,
                 clock: Callable[[], float] = time.time, domain: str = "") -> None:
        self._client = client
        self._domain = domain
        self._timeout = self.publish_timeout = publish_timeout
        self._max_age, self._max_msgs = max_age, max_msgs
        self._clock = clock

    @classmethod
    async def connect(cls, dsn: str, **kw: Any) -> RedisPublisher:
        """``redis://`` / ``rediss://``; ``cluster=true`` selects a cluster client
        (lib/redisdsn)."""
        from urllib.parse import parse_qs, urlencode, urlsplit, urlunsplit

        u = urlsplit(dsn)
        if u.scheme not in ("redis", "rediss"):
            raise ValueError(f"redis DSN: scheme {u.scheme!r}, want redis or rediss")
        q = parse_qs(u.query, keep_blank_values=True)
        cluster = False
        if "cluster" in q:
            vals = q.pop("cluster")
            if len(vals) != 1 or vals[0].lower() not in ("1", "t", "true", "0", "f", "false"):
                raise ValueError("redis DSN: cluster=<bool> given more than once or not a bool")
            cluster = vals[0].lower() in ("1", "t", "true")
            if cluster and u.path not in ("", "/", "/0"):
                raise ValueError("redis DSN: a cluster has database 0 only")
            dsn = urlunsplit((u.scheme, u.netloc, "" if cluster else u.path, urlencode(q, doseq=True), u.fragment))
        if cluster:
            from redis.asyncio.cluster import RedisCluster

            client = RedisCluster.from_url(dsn)
        else:
            import redis.asyncio as aredis

            client = aredis.from_url(dsn)
        await asyncio.wait_for(client.ping(), 5.0)
        return cls(client, **kw)

    async def dispatch(self, ctx: Context, channel: str, topic: str, envelope: Message) -> None:
        validate_channel(channel)
        validate_topic(topic)
        raw = envelope.SerializeToString()
        kw: dict[str, Any] = {}
        if self._max_age > 0:
            kw["minid"] = f"{int((self._clock() - self._max_age) * 1000)}-0"
            kw["approximate"] = True
        elif self._max_msgs > 0:
            kw["maxlen"] = self._max_msgs
            kw["approximate"] = True

        async def go() -> None:
            fields: dict[str, Any] = {"topic": topic, "payload": raw}
            if self._domain:
                fields[REDIS_FIELD_DOMAIN] = self._domain
            await self._client.xadd(channel, fields, **kw)
            if self._max_age > 0 and self._max_msgs > 0:
                try:
                    await self._client.xtrim(channel, maxlen=self._max_msgs, approximate=True)
                except Exception:  # noqa: BLE001 — best effort, the next append trims again
                    pass

        await asyncio.wait_for(go(), self._timeout)

    async def close(self) -> None:
        await self._client.aclose()


class Router:
    """Go's eventbus ``Route``: a channel declared on Redis Streams goes to the
    Redis bus, every other channel to the NATS bus. When the bus a channel
    belongs to is not configured (its DSN is empty) the event is dropped —
    never sent to the other bus, because the subscriber reads each channel
    from its declared transport only."""

    def __init__(self, nats: Dispatcher | None = None, redis: Dispatcher | None = None,
                 redis_channels: tuple[str, ...] = ()) -> None:
        self.nats = nats
        self.redis = redis
        self.redis_channels = frozenset(redis_channels)

    def for_channel(self, channel: str) -> Dispatcher | None:
        return self.redis if channel in self.redis_channels else self.nats

    async def dispatch(self, ctx: Context, channel: str, topic: str, envelope: Message) -> None:
        d = self.for_channel(channel)
        if d is None:
            return
        await d.dispatch(ctx, channel, topic, envelope)

    async def close(self) -> None:
        for d in (self.nats, self.redis):
            if d is not None:
                await d.close()


W17_INVALIDATE_TOPIC = "w17.invalidate"


def invalidate_events(pool: Pool, inv: Invalidate, ctx: Context) -> list[Message]:
    """eventbus.EmitInvalidate per entity: a bare ``w17.InvalidateEvent``
    (not wrapped in the envelope) with the request's labels and no ids."""
    cls = pool.message("w17.InvalidateEvent")
    labels = labels_from_context(ctx) or {}
    out = []
    for entity in inv.entities:
        ev = cls(group=inv.group, message=entity)
        ev.labels.update(labels)
        out.append(ev)
    return out


async def fire(dispatcher: Dispatcher | None, ctx: Context, channel: str, topic: str, envelope: Message,
               what: str = "") -> None:
    """Publish fire-and-forget: a failure is reported, never raised (the RPC
    already succeeded). Runs detached from the request's cancellation."""
    if dispatcher is None:
        return
    # Go bounds each Dispatch by the adapter's PublishTimeout: the emit runs on
    # the committing path, and a broker that stopped answering must not hold
    # it (nor, here, the drain) indefinitely. Any dispatcher gets the bound.
    timeout = getattr(dispatcher, "publish_timeout", None) or DEFAULT_PUBLISH_TIMEOUT
    try:
        await asyncio.wait_for(dispatcher.dispatch(ctx.without_cancel(), channel, topic, envelope), timeout)
    except Exception as e:  # noqa: BLE001
        report_error(ctx, RuntimeError(f"eventbus: emit {channel}/{topic}{' for ' + what if what else ''} failed: {e!r}"))
