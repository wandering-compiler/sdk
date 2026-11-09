import { afterEach, describe, expect, it, vi } from "vitest";
import { Context } from "../src/context.ts";
import * as events from "../src/events.ts";
import type { Emit, Invalidate } from "../src/manifest.ts";
import { msg, pool } from "./helpers.ts";

const EMIT: Emit = {
  method: "/conformance.v1.NoteService/CreateNote",
  channel: "notes",
  topic: "note.created",
  envelope: "conformance.events.Event",
  envelopeField: "v1_note_created",
  event: "conformance.v1.NoteCreated",
  fields: [
    { field: "id", source: "response", path: ["id"], value: "" },
    { field: "text", source: "request", path: ["text"], value: "" },
    { field: "by", source: "literal", path: [], value: "conformance" },
  ],
};

const Event = () => pool().message("conformance.events.Event");
const EMPTY = new Uint8Array();

afterEach(() => vi.restoreAllMocks());

describe("credentials and reserved keys", () => {
  it.each([
    "authorization", "Authorization", "cookie", "set-cookie", "proxy-authorization", "x-w17-credential",
    "x-api-key", "x-auth-token", "x-amz-security-token", "x-session-id", "x-csrf-token", "x-client-secret",
    "grpcgateway-cookie", "grpcgateway-authorization", "my.jwt", "x-apikey",
  ])("%s is stripped", (k) => expect(events.reservedMetadataKey(k)).toBe(true));

  it.each(["x-author-id", "x-w17-scope-session_id", "x-w17-user", "w17-label-token_kind", "traceparent", "w17-language"])(
    "%s travels",
    (k) => expect(events.reservedMetadataKey(k)).toBe(false),
  );

  it("transaction keys never ride an envelope", () => {
    expect(events.reservedMetadataKey("w17-tx-id")).toBe(true);
    expect(events.reservedMetadataKey("W17-Conn-Id")).toBe(true);
  });
});

describe("context data", () => {
  it("gateway-owned keys keep their first value, the others their last", () => {
    const ctx = new Context({
      incoming: [
        ["x-w17-user", "first"], ["x-w17-user", "second"],
        ["w17-label-t", "a"], ["w17-label-t", "b"],
        ["w17-language", "en"], ["w17-language", "cs"],
        ["w17-tx-id", "tx"], ["authorization", "Bearer x"],
      ],
    });
    expect(events.metadataFromContext(ctx)).toEqual({ "x-w17-user": "first", "w17-label-t": "a", "w17-language": "cs" });
    expect(events.labelsFromContext(ctx)).toEqual({ t: "a" });
  });

  it("incoming first, outgoing second, never merged", () => {
    const ctx = new Context({ incoming: { a: "in" }, outgoing: { b: "out", "w17-label-x": "1" } });
    expect(events.metadataFromContext(ctx)).toEqual({ a: "in" });
    expect(events.labelsFromContext(ctx)).toBeUndefined();
    const onlyOut = new Context({ outgoing: { b: "out", "w17-label-x": "1" } });
    expect(events.metadataFromContext(onlyOut)).toEqual({ b: "out", "w17-label-x": "1" });
    expect(events.labelsFromContext(onlyOut)).toEqual({ x: "1" });
    expect(events.metadataFromContext(new Context())).toBeUndefined();
  });

  it("a bare label prefix is not a label", () => {
    expect(events.labelsFromContext(new Context({ incoming: { "w17-label-": "x" } }))).toBeUndefined();
  });
});

describe("names", () => {
  it.each(["", "a.b", "a b", "w17", "W17_requests", "x".repeat(201), "ä"])("channel %j is invalid", (name) => {
    expect(() => events.validateChannel(name)).toThrow(events.InvalidChannelError);
  });

  it.each(["", "a..b", "a.*", "a.>", "a b", "a\tb"])("topic %j is invalid", (t) => {
    expect(() => events.validateTopic(t)).toThrow(events.InvalidTopicError);
  });

  it("valid names pass", () => {
    events.validateChannel("notes_main-1");
    events.validateTopic("note.created");
  });
});

describe("the envelope", () => {
  it("is Go's shape", () => {
    const b = new events.EnvelopeBuilder(pool(), EMIT);
    const ctx = new Context({ incoming: { "x-w17-user": "e", "w17-label-tenant": "t1", authorization: "x", "w17-tx-id": "tx" } });
    const env = b.build(ctx, msg("CreateNoteRequest")({ text: "hi" }), msg("Note")({ id: 7n, text: "ignored" }));
    expect(env.payload.case).toBe("v1NoteCreated");
    const ev = env.payload.value;
    expect([ev.id, ev.text, ev.by]).toEqual([7n, "hi", "conformance"]);
    expect(env.metadata).toEqual({ "x-w17-user": "e", "w17-label-tenant": "t1" });
    expect(env.labels).toEqual({ tenant: "t1" });
    // Field numbers are the lock's: metadata 50001, labels 50002.
    const d = Event().schema;
    expect(d.fields.find((f) => f.name === "metadata")!.number).toBe(50001);
    expect(d.fields.find((f) => f.name === "labels")!.number).toBe(50002);
    // And it decodes back to the same thing.
    const back = Event().fromBinary(b.marshal(env));
    expect(back.payload.value.text).toBe("hi");
  });

  it("is written in field-number order, as Go marshals it", () => {
    const b = new events.EnvelopeBuilder(pool(), EMIT);
    const env = b.build(new Context({ incoming: { a: "1", "w17-label-t": "x" } }), msg("CreateNoteRequest")({ text: "t" }), msg("Note")({ id: 1n }));
    const raw = b.marshal(env);
    // The payload (field 1) first: tag 0x0a; then metadata (50001) and labels (50002).
    expect(raw[0]).toBe(0x0a);
  });

  it("a wrong manifest is refused", () => {
    expect(() => new events.EnvelopeBuilder(pool(), { ...EMIT, envelopeField: "nope" })).toThrow(/payload case/);
    expect(() => new events.EnvelopeBuilder(pool(), { ...EMIT, fields: [{ field: "nope", source: "literal", path: [], value: "" }] })).toThrow(/no field/);
  });

  it("a payload read through an unset submessage reads zero", () => {
    const emit: Emit = { ...EMIT, fields: [{ field: "id", source: "response", path: ["paging", "total"], value: "" }] };
    const env = new events.EnvelopeBuilder(pool(), emit).build(new Context(), msg("ListNotesRequest")(), msg("ListNotesResponse")());
    expect(env.payload.value.id).toBe(0n);
  });
});

// --- brokers ---------------------------------------------------------------

class FakeRedis implements events.RedisLike {
  calls: unknown[][] = [];
  async xadd(...args: unknown[]) {
    this.calls.push(["xadd", ...args]);
  }
  async xtrim(...args: unknown[]) {
    this.calls.push(["xtrim", ...args]);
  }
  async quit() {}
}

describe("redis", () => {
  it("writes what Go writes", async () => {
    const r = new FakeRedis();
    const pub = new events.RedisPublisher(r, { clock: () => 1_000_000_000 });
    const raw = new Uint8Array([1, 2, 3]);
    await pub.dispatch(new Context(), "notes", "note.created", raw);
    expect(r.calls[0]).toEqual([
      "xadd", "notes", "MINID", "~", `${1_000_000_000 - 7 * 24 * 3600 * 1000}-0`, "*", "topic", "note.created", "payload", Buffer.from(raw),
    ]);
  });

  it("a count limit trims by length", async () => {
    const r = new FakeRedis();
    const pub = new events.RedisPublisher(r, { maxAgeMs: -1, maxMsgs: 10 });
    await pub.dispatch(new Context(), "notes", "t", EMPTY);
    expect(r.calls[0]!.slice(0, 5)).toEqual(["xadd", "notes", "MAXLEN", "~", 10]);
  });

  it("the DSN is validated", async () => {
    await expect(events.RedisPublisher.connect("http://x")).rejects.toThrow(/scheme/);
    await expect(events.RedisPublisher.connect("redis://h:6379/3?cluster=true")).rejects.toThrow(/database 0/);
  });
});

class FakeJS implements events.JetStreamLike {
  streams = new Map<string, events.StreamSpec>();
  published: [string, Uint8Array][] = [];
  loseStream: boolean;
  constructor(loseStream = false) {
    this.loseStream = loseStream;
  }
  async updateStream(cfg: events.StreamSpec) {
    if (!this.streams.has(cfg.name)) throw new events.StreamMissingError("not found");
    this.streams.set(cfg.name, cfg);
  }
  async addStream(cfg: events.StreamSpec) {
    this.streams.set(cfg.name, cfg);
  }
  async publish(subject: string, payload: Uint8Array) {
    if (this.loseStream) {
      this.loseStream = false;
      throw new events.StreamMissingError("no responders");
    }
    this.published.push([subject, payload]);
  }
  isClosed() {
    return false;
  }
  async close() {}
}

describe("nats", () => {
  it("the stream and the subject are Go's", async () => {
    const js = new FakeJS();
    await new events.NatsPublisher(js).dispatch(new Context(), "notes", "note.created", EMPTY);
    expect(js.streams.get("notes")).toEqual({
      name: "notes", subjects: ["notes.>"], retention: "limits", discard: "old", maxAgeMs: 7 * 24 * 3600 * 1000, maxMsgs: -1,
    });
    expect(js.published).toEqual([["notes.note.created", EMPTY]]);
  });

  it("a lost stream is re-created", async () => {
    const js = new FakeJS(true);
    await new events.NatsPublisher(js).dispatch(new Context(), "notes", "t", EMPTY);
    expect(js.published).toEqual([["notes.t", EMPTY]]);
  });

  it("the DSN scheme is checked", async () => {
    await expect(events.NatsPublisher.connect("http://x")).rejects.toThrow(/nats:\/\/ or tls:\/\//);
    await expect(events.NatsPublisher.connect("nats://a:4222,http://b")).rejects.toThrow(/nats:\/\/ or tls:\/\//);
  });

  it("the DSN reads as nats.go reads it", () => {
    const o = events.natsConnectOptions;
    // A user with no password is a token.
    expect(o("nats://s3cr%40t@nats")).toEqual({ servers: ["nats:4222"], token: "s3cr@t", tls: false });
    expect(o("nats://u:p%3Aw@nats:4223")).toEqual({ servers: ["nats:4223"], user: "u", pass: "p:w", tls: false });
    expect(o("nats://u:@nats")).toEqual({ servers: ["nats:4222"], user: "u", pass: "", tls: false });
    // A server list keeps every server; tls:// anywhere is TLS.
    expect(o("nats://t@a:4222, tls://b:4333,c")).toEqual({ servers: ["a:4222", "b:4333", "c:4222"], token: "t", tls: true });
    expect(o("tls://[::1]:4222")).toEqual({ servers: ["[::1]:4222"], tls: true });
    expect(() => o("nats://")).toThrow(RangeError);
  });
});

describe("fire and route", () => {
  it("fire never throws", async () => {
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    const broken: events.Dispatcher = { dispatch: () => Promise.reject(new Error("broker down")), close: async () => {} };
    await events.fire(broken, new Context(), "notes", "t", EMPTY);
    await events.fire(undefined, new Context(), "notes", "t", EMPTY);
  });

  it("each channel goes to its declared bus only", async () => {
    const rec = () => {
      const got: [string, string][] = [];
      return { got, dispatch: async (_c: Context, ch: string, t: string) => void got.push([ch, t]), close: async () => {} };
    };
    const nats = rec();
    const redis = rec();
    const r = new events.Router(nats, redis, ["audit"]);
    await r.dispatch(new Context(), "audit", "t", EMPTY);
    await r.dispatch(new Context(), "notes", "t", EMPTY);
    expect(redis.got).toEqual([["audit", "t"]]);
    expect(nats.got).toEqual([["notes", "t"]]);
    // A bus that is off drops its channels' events; they never go to the other bus.
    await new events.Router(nats, undefined, ["audit"]).dispatch(new Context(), "audit", "t", EMPTY);
    expect(nats.got).toEqual([["notes", "t"]]);
  });

  it("invalidate events are bare and labelled", () => {
    const inv: Invalidate = { method: "/a.S/M", channel: "notes", topic: "w17.invalidate", group: "conformance.v1", entities: ["Note", "Tag"] };
    const evs = events.invalidateEvents(pool(), inv, new Context({ incoming: { "w17-label-tenant": "t" } }));
    expect(evs.map((e) => [e.group, e.message, e.ids, e.labels])).toEqual([
      ["conformance.v1", "Note", [], { tenant: "t" }],
      ["conformance.v1", "Tag", [], { tenant: "t" }],
    ]);
    expect(evs[0]!.$typeName).toBe("w17.InvalidateEvent");
  });
});
