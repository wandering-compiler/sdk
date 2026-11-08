import pytest

from w17_business import events
from w17_business.context import Context
from w17_business.manifest import Emit, PayloadBinding

EMIT = Emit(
    method="/conformance.v1.NoteService/CreateNote",
    channel="notes",
    topic="note.created",
    envelope="conformance.events.Event",
    envelope_field="v1_note_created",
    event="conformance.v1.NoteCreated",
    fields=(
        PayloadBinding(field="id", source="response", path=("id",)),
        PayloadBinding(field="text", source="request", path=("text",)),
        PayloadBinding(field="by", source="literal", value="conformance"),
    ),
)


@pytest.mark.parametrize("key", [
    "authorization", "Authorization", "cookie", "set-cookie", "proxy-authorization", "x-w17-credential",
    "x-api-key", "x-auth-token", "x-amz-security-token", "x-session-id", "x-csrf-token", "x-client-secret",
    "grpcgateway-cookie", "grpcgateway-authorization", "my.jwt", "x-apikey",
])
def test_credentials_are_stripped(key):
    assert events.reserved_metadata_key(key)


@pytest.mark.parametrize("key", [
    "x-author-id", "x-w17-scope-session_id", "x-w17-user", "w17-label-token_kind", "traceparent", "w17-language",
])
def test_ordinary_and_gateway_owned_keys_travel(key):
    assert not events.reserved_metadata_key(key)


def test_tx_keys_never_ride_an_envelope():
    assert events.reserved_metadata_key("w17-tx-id")
    assert events.reserved_metadata_key("W17-Conn-Id")


def test_metadata_first_for_gateway_owned_last_for_others():
    ctx = Context(incoming=[
        ("x-w17-user", "first"), ("x-w17-user", "second"),
        ("w17-label-t", "a"), ("w17-label-t", "b"),
        ("w17-language", "en"), ("w17-language", "cs"),
        ("w17-tx-id", "tx"), ("authorization", "Bearer x"),
    ])
    md = events.metadata_from_context(ctx)
    assert md == {"x-w17-user": "first", "w17-label-t": "a", "w17-language": "cs"}
    assert events.labels_from_context(ctx) == {"t": "a"}


def test_incoming_first_outgoing_second_never_merged():
    ctx = Context(incoming={"a": "in"}, outgoing={"b": "out", "w17-label-x": "1"})
    assert events.metadata_from_context(ctx) == {"a": "in"}
    assert events.labels_from_context(ctx) is None
    only_out = Context(outgoing={"b": "out", "w17-label-x": "1"})
    assert events.metadata_from_context(only_out) == {"b": "out", "w17-label-x": "1"}
    assert events.labels_from_context(only_out) == {"x": "1"}
    assert events.metadata_from_context(Context()) is None


def test_bare_label_prefix_is_not_a_label():
    assert events.labels_from_context(Context(incoming={"w17-label-": "x"})) is None


@pytest.mark.parametrize("name", ["", "a.b", "a b", "w17", "W17_requests", "x" * 201, "ä"])
def test_invalid_channels(name):
    with pytest.raises(events.InvalidChannelError):
        events.validate_channel(name)


@pytest.mark.parametrize("topic", ["", "a..b", "a.*", "a.>", "a b", "a\tb"])
def test_invalid_topics(topic):
    with pytest.raises(events.InvalidTopicError):
        events.validate_topic(topic)


def test_valid_names():
    events.validate_channel("notes_main-1")
    events.validate_topic("note.created")


def test_envelope_is_the_go_shape(pool, msg):
    b = events.EnvelopeBuilder(pool, EMIT)
    ctx = Context(incoming={"x-w17-user": "e", "w17-label-tenant": "t1", "authorization": "x", "w17-tx-id": "tx"})
    env = b.build(ctx, msg("CreateNoteRequest")(text="hi"), msg("Note")(id=7, text="ignored"))
    assert env.WhichOneof("payload") == "v1_note_created"
    ev = env.v1_note_created
    assert (ev.id, ev.text, ev.by) == (7, "hi", "conformance")
    assert dict(env.metadata) == {"x-w17-user": "e", "w17-label-tenant": "t1"}
    assert dict(env.labels) == {"tenant": "t1"}
    # Field numbers are the lock's: metadata 50001, labels 50002.
    d = env.DESCRIPTOR
    assert d.fields_by_name["metadata"].number == 50001
    assert d.fields_by_name["labels"].number == 50002


def test_envelope_builder_refuses_a_wrong_manifest(pool):
    bad = Emit(**{**EMIT.__dict__, "envelope_field": "nope"})
    with pytest.raises(ValueError, match="payload case"):
        events.EnvelopeBuilder(pool, bad)
    bad = Emit(**{**EMIT.__dict__, "fields": (PayloadBinding(field="nope", source="literal"),)})
    with pytest.raises(ValueError, match="no field"):
        events.EnvelopeBuilder(pool, bad)


def test_payload_from_an_unset_submessage_reads_zero(pool, msg):
    emit = Emit(**{**EMIT.__dict__, "fields": (
        PayloadBinding(field="id", source="response", path=("paging", "total")),
    ), "event": "conformance.v1.NoteCreated"})
    b = events.EnvelopeBuilder(pool, emit)
    env = b.build(Context(), msg("ListNotesRequest")(), msg("ListNotesResponse")())
    assert env.v1_note_created.id == 0


class FakeRedis:
    def __init__(self):
        self.calls = []

    async def xadd(self, name, fields, **kw):
        self.calls.append(("xadd", name, fields, kw))

    async def xtrim(self, name, **kw):
        self.calls.append(("xtrim", name, kw))

    async def aclose(self):
        pass


async def test_redis_publisher_writes_what_go_writes(pool, msg):
    r = FakeRedis()
    pub = events.RedisPublisher(r, clock=lambda: 1_000_000.0)
    env = events.EnvelopeBuilder(pool, EMIT).build(Context(), msg("CreateNoteRequest")(text="t"), msg("Note")(id=1))
    await pub.dispatch(Context(), "notes", "note.created", env)
    op, stream, fields, kw = r.calls[0]
    assert stream == "notes"
    assert fields == {"topic": "note.created", "payload": env.SerializeToString()}
    assert kw == {"minid": f"{int((1_000_000.0 - 7 * 24 * 3600) * 1000)}-0", "approximate": True}


async def test_redis_publisher_count_limit():
    r = FakeRedis()
    pub = events.RedisPublisher(r, max_age=-1, max_msgs=10)
    from google.protobuf import empty_pb2

    await pub.dispatch(Context(), "notes", "t", empty_pb2.Empty())
    assert r.calls[0][3] == {"maxlen": 10, "approximate": True}


async def test_redis_dsn_validation():
    with pytest.raises(ValueError, match="scheme"):
        await events.RedisPublisher.connect("http://x")
    with pytest.raises(ValueError, match="database 0"):
        await events.RedisPublisher.connect("redis://h:6379/3?cluster=true")


class FakeJS:
    def __init__(self, lose_stream=False):
        self.streams = {}
        self.published = []
        self.lose_stream = lose_stream

    async def update_stream(self, cfg):
        from nats.js.errors import NotFoundError

        if cfg.name not in self.streams:
            raise NotFoundError()
        self.streams[cfg.name] = cfg

    async def add_stream(self, cfg):
        self.streams[cfg.name] = cfg

    async def publish(self, subject, payload, timeout=None):
        from nats.js.errors import NoStreamResponseError

        if self.lose_stream:
            self.lose_stream = False
            raise NoStreamResponseError()
        self.published.append((subject, payload))


class FakeNC:
    is_closed = False


async def test_nats_publisher_stream_and_subject_are_gos():
    from google.protobuf import empty_pb2
    from nats.js.api import DiscardPolicy, RetentionPolicy

    js = FakeJS()
    pub = events.NatsPublisher(FakeNC(), js)
    await pub.dispatch(Context(), "notes", "note.created", empty_pb2.Empty())
    cfg = js.streams["notes"]
    assert cfg.subjects == ["notes.>"]
    assert cfg.retention == RetentionPolicy.LIMITS and cfg.discard == DiscardPolicy.OLD
    assert cfg.max_age == 7 * 24 * 3600 and cfg.max_msgs == -1
    assert js.published == [("notes.note.created", b"")]


async def test_nats_publisher_recreates_a_lost_stream():
    from google.protobuf import empty_pb2

    js = FakeJS(lose_stream=True)
    pub = events.NatsPublisher(FakeNC(), js)
    await pub.dispatch(Context(), "notes", "t", empty_pb2.Empty())
    assert js.published == [("notes.t", b"")]


async def test_fire_never_raises():
    class Broken:
        async def dispatch(self, *a):
            raise ConnectionError("broker down")

    from google.protobuf import empty_pb2

    await events.fire(Broken(), Context(), "notes", "t", empty_pb2.Empty())  # no exception
    await events.fire(None, Context(), "notes", "t", empty_pb2.Empty())


class Rec:
    def __init__(self):
        self.got = []

    async def dispatch(self, ctx, channel, topic, env):
        self.got.append((channel, topic))

    async def close(self):
        pass


async def test_router_sends_each_channel_to_its_declared_bus_only():
    from google.protobuf import empty_pb2

    nats, redis = Rec(), Rec()
    r = events.Router(nats, redis, ("audit",))
    await r.dispatch(Context(), "audit", "t", empty_pb2.Empty())
    await r.dispatch(Context(), "notes", "t", empty_pb2.Empty())
    assert redis.got == [("audit", "t")] and nats.got == [("notes", "t")]
    # A bus that is off drops its channels' events; they never go to the other bus.
    only_nats = events.Router(nats, None, ("audit",))
    await only_nats.dispatch(Context(), "audit", "t", empty_pb2.Empty())
    assert nats.got == [("notes", "t")]


def test_invalidate_events_are_bare_and_labelled(pool):
    from w17_business.manifest import Invalidate

    inv = Invalidate("/a.S/M", "notes", "w17.invalidate", "conformance.v1", ("Note", "Tag"))
    evs = events.invalidate_events(pool, inv, Context(incoming={"w17-label-tenant": "t"}))
    assert [(e.group, e.message, list(e.ids), dict(e.labels)) for e in evs] == [
        ("conformance.v1", "Note", [], {"tenant": "t"}),
        ("conformance.v1", "Tag", [], {"tenant": "t"}),
    ]
    assert evs[0].DESCRIPTOR.full_name == "w17.InvalidateEvent"
