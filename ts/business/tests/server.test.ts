// The served tier end to end, over real grpc-js servers on loopback: a
// TypeScript stand-in for storage (served by the same Registry) and the
// business service.

import { create, toBinary } from "@bufbuild/protobuf";
import { FileDescriptorProtoSchema, FileDescriptorSetSchema } from "@bufbuild/protobuf/wkt";
import { Client, Metadata as GrpcMetadata, Server, ServerCredentials, credentials, type ServiceError } from "@grpc/grpc-js";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { Conn, ServiceClient, TxCoordinator } from "../src/client.ts";
import { Context, Metadata } from "../src/context.ts";
import type { AnyMessage, Pool } from "../src/descriptors.ts";
import * as distx from "../src/distx.ts";
import { Status, StatusError, errorDetail, forUser } from "../src/errors.ts";
import * as events from "../src/events.ts";
import type { Emit } from "../src/manifest.ts";
import * as observability from "../src/observability.ts";
import * as paging from "../src/paging.ts";
import * as principal from "../src/principal.ts";
import { RegistrationError, Registry, methodDefinition, requests, toServiceError } from "../src/server.ts";
import { serializer } from "../src/client.ts";
import * as txscope from "../src/txscope.ts";
import { msg, pool, settled, sleep } from "./helpers.ts";

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
    { field: "by", source: "literal", path: [], value: "ts" },
  ],
};

const DM = (name: string, init: Record<string, unknown> = {}) => pool().message("w17.common.distx." + name)(init);

/**
 * Storage stand-in: notes in memory, a coordinator that stages writes per
 * transaction, and a record of what each call carried.
 */
class FakeStorage {
  rows: string[] = [];
  staged = new Map<string, string[]>();
  seen: [string, Context][] = [];
  /** Answered UNAVAILABLE by the handler, after its write. */
  handlerUnavailable = 0;
  n = 0;

  async InsertNote(ctx: Context, req: AnyMessage) {
    this.seen.push(["InsertNote", ctx]);
    this.n++;
    const note = msg("Note")({ id: BigInt(this.n), text: req.text });
    const tx = ctx.incoming.first("w17-tx-id");
    if (tx) {
      const l = this.staged.get(tx) ?? [];
      l.push(note.text);
      this.staged.set(tx, l);
    } else this.rows.push(note.text);
    if (this.handlerUnavailable) {
      this.handlerUnavailable--;
      throw new StatusError(Status.UNAVAILABLE, "the upstream went away after the write");
    }
    return note;
  }

  async ListNotes(ctx: Context, _req: AnyMessage) {
    this.seen.push(["ListNotes", ctx]);
    const limit = Number(ctx.incoming.first(paging.LIMIT_MD_KEY) ?? 50);
    const resp = msg("ListNotesResponse")({
      notes: this.rows.slice(0, limit).map((t, i) => msg("Note")({ id: BigInt(i + 1), text: t })),
      paging: pool().message("w17.Paging")({ limit }),
    });
    // The page position, as a generated storage handler reports it.
    const sink = paging.responseHeaders(ctx);
    sink?.set(paging.NEXT_MD_KEY, ["next-of-page"]);
    sink?.set(paging.PREV_MD_KEY, ["prev-of-page"]);
    sink?.set(paging.TOTAL_MD_KEY, [String(this.rows.length)]);
    return resp;
  }

  async Inspect(ctx: Context, _req: AnyMessage) {
    this.seen.push(["Inspect", ctx]);
    // A sibling call's position, which must not reach the caller.
    paging.responseHeaders(ctx)?.set(paging.NEXT_MD_KEY, ["sibling"]);
    const rem = ctx.timeRemaining();
    return msg("InspectResponse")({
      headers: [...ctx.incoming].map(([key, values]) => msg("Header")({ key, values: [...values] })),
      ...(rem !== undefined && { hasDeadline: true, deadlineMs: BigInt(Math.trunc(rem)) }),
    });
  }
}

class FakeCoordinator {
  n = 0;
  calls: string[] = [];
  readonly s: FakeStorage;
  constructor(s: FakeStorage) {
    this.s = s;
  }
  async Begin(_ctx: Context, _req: AnyMessage) {
    this.n++;
    this.calls.push("begin");
    return DM("BeginResponse", { txId: `tx-${this.n}` });
  }
  async Commit(_ctx: Context, req: AnyMessage) {
    this.calls.push("commit");
    this.s.rows.push(...(this.s.staged.get(req.txId) ?? []));
    this.s.staged.delete(req.txId);
    return DM("CommitResponse");
  }
  async Rollback(_ctx: Context, req: AnyMessage) {
    this.calls.push("rollback");
    this.s.staged.delete(req.txId);
    return DM("RollbackResponse");
  }
}

class NoteService {
  readonly registry: Registry;
  readonly storage: ServiceClient | undefined;
  readonly tx: distx.TxClient | undefined;
  constructor(registry: Registry, storage: ServiceClient | undefined, tx: distx.TxClient | undefined) {
    this.registry = registry;
    this.storage = storage;
    this.tx = tx;
  }
  M = (n: string) => this.registry.pool.message("conformance.v1." + n);

  async CreateNote(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    return distx.run(ctx, this.tx, null, (c) => this.storage!.InsertNote(c, this.M("InsertNoteRequest")({ text: req.text })));
  }
  async CreateNotes(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    const me = this.registry.self("conformance.v1.NoteService");
    const out = this.M("CreateNotesResponse")();
    await distx.run(ctx, this.tx, null, async (c) => {
      for (const t of req.texts as string[]) out.notes.push(await me.CreateNote(c, this.M("CreateNoteRequest")({ text: t })));
      if (req.fail) throw new Error("refuse after the writes");
    });
    return out;
  }
  async ListNotes(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    await this.storage!.Inspect(ctx, this.M("InspectRequest")());
    return this.storage!.ListNotes(paging.forward(ctx), req);
  }
  async Inspect(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    return this.storage!.Inspect(ctx, req);
  }
  async Refuse(): Promise<AnyMessage> {
    throw forUser(Status.FAILED_PRECONDITION, "NOTE_LOCKED", "This note is locked.", "text", "locked by export");
  }
  async Sleep(_ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    await sleep(req.ms);
    return this.M("Note")({ text: "slept" });
  }
  async TxErrors(): Promise<AnyMessage> {
    throw new RangeError("a bug");
  }
  async Whoami(ctx: Context): Promise<AnyMessage> {
    return this.M("WhoamiResponse")({ userId: principal.userId(ctx) });
  }
  // The companion's methods: the suite's NoteService has them; this file
  // tests the SDK without a companion (companion.test.ts has one).
  async Echo(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "no companion here");
  }
  async AttachFile(): Promise<AnyMessage> {
    throw new StatusError(Status.UNIMPLEMENTED, "no companion here");
  }
}

async function listen(reg: Registry): Promise<[Server, number]> {
  const s = new Server();
  reg.addTo(s);
  const port = await new Promise<number>((res, rej) =>
    s.bindAsync("127.0.0.1:0", ServerCredentials.createInsecure(), (e, p) => (e ? rej(e) : res(p))),
  );
  return [s, port];
}

function stop(s: Server): Promise<void> {
  return new Promise((r) => s.tryShutdown(() => r()));
}

interface Stack {
  storage: FakeStorage;
  coord: FakeCoordinator;
  pub: events.MemoryPublisher;
  client: ServiceClient;
  registry: Registry;
  conn: Conn;
  bconn: Conn;
  servers: Server[];
}

let stack: Stack;

beforeAll(async () => {
  const p = pool();
  const storage = new FakeStorage();
  const sreg = new Registry(p);
  sreg.register("conformance.v1.NoteStorage", storage);
  const coord = new FakeCoordinator(storage);
  sreg.register("w17.common.distx.W17DistributedTransaction", coord);
  const [ssrv, sport] = await listen(sreg);
  const conn = new Conn(`127.0.0.1:${sport}`, { tls: false });
  const pub = new events.MemoryPublisher();
  const breg = new Registry(p, { emits: [EMIT], dispatcher: pub });
  breg.register("NoteService", new NoteService(breg, new ServiceClient(conn, "conformance.v1.NoteStorage", p), new TxCoordinator(conn)));
  const [bsrv, bport] = await listen(breg);
  const bconn = new Conn(`127.0.0.1:${bport}`, { tls: false });
  stack = {
    storage, coord, pub, registry: breg, conn, bconn, servers: [ssrv, bsrv],
    client: new ServiceClient(bconn, "conformance.v1.NoteService", p),
  };
});

afterAll(async () => {
  stack.conn.close();
  stack.bconn.close();
  await Promise.all(stack.servers.map(stop));
});

afterEach(() => {
  stack.storage.rows = [];
  stack.storage.seen = [];
  stack.coord.calls = [];
  stack.pub.published.length = 0;
  principal.setAuthRespType(undefined);
  vi.restoreAllMocks();
});

const headers = (resp: AnyMessage) => Object.fromEntries((resp.headers as AnyMessage[]).map((h) => [h.key, h.values]));
const Event = () => pool().message("conformance.events.Event");

async function statusOf(p: Promise<unknown>): Promise<StatusError> {
  try {
    await p;
  } catch (e) {
    expect(e).toBeInstanceOf(StatusError);
    return e as StatusError;
  }
  throw new Error("the call succeeded; want an error");
}

describe("over the wire", () => {
  it("forwards exactly the principal headers", async () => {
    const env = Buffer.from(msg("AuthResp").toBinary(msg("AuthResp")({ userId: "u-1" }))).toString("base64");
    const ctx = new Context({
      outgoing: {
        "x-w17-user": env, "x-w17-scope-org_id": "o-1", "w17-label-tenant": "t", "x-w17-device": "d",
        "x-w17-data-scope": "org", authorization: "Bearer nope", "x-w17-paging-limit": "2",
        "w17-language": "cs", "x-custom": "c",
      },
    });
    const h = headers(await stack.client.Inspect(ctx, msg("InspectRequest")()));
    for (const k of ["x-w17-user", "x-w17-scope-org_id", "w17-label-tenant", "x-w17-device", "x-w17-data-scope"]) expect(h).toHaveProperty([k]);
    for (const k of ["authorization", "x-w17-paging-limit", "w17-language", "x-custom"]) expect(h).not.toHaveProperty([k]);
  });

  it("the deadline propagates and expires", async () => {
    const resp = await stack.client.Inspect(new Context().withTimeout(5000), msg("InspectRequest")());
    expect(resp.hasDeadline).toBe(true);
    expect(Number(resp.deadlineMs)).toBeGreaterThan(0);
    expect(Number(resp.deadlineMs)).toBeLessThanOrEqual(5000);
    const st = await statusOf(stack.client.Sleep(new Context().withTimeout(200), msg("SleepRequest")({ ms: 3000 })));
    expect(st.code).toBe(Status.DEADLINE_EXCEEDED);
  });

  it("a passed deadline reads as the deadline in the handler, not as a cancel", async () => {
    const seen: unknown[] = [];
    class Late extends NoteService {
      override async Sleep(ctx: Context, _req: AnyMessage): Promise<AnyMessage> {
        await new Promise<void>((r) => ctx.onDone(r));
        await sleep(50); // grpc-js's 'cancelled' for the DEADLINE_EXCEEDED has come by now
        seen.push(ctx.err());
        try {
          await this.storage!.Inspect(ctx, this.M("InspectRequest")());
        } catch (e) {
          seen.push((e as StatusError).code);
        }
        try {
          await distx.run(ctx, { begin: async () => ({ reply: DM("BeginResponse", { txId: "t" }), header: new Metadata() }), commit: async () => DM("CommitResponse"), rollback: async () => DM("RollbackResponse") }, null, () => 1);
        } catch (e) {
          seen.push((e as Error).message);
        }
        return this.M("Note")();
      }
    }
    const reg = new Registry(pool());
    reg.register("NoteService", new Late(reg, new ServiceClient(stack.conn, "conformance.v1.NoteStorage", pool()), undefined));
    const [srv, port] = await listen(reg);
    const conn = new Conn(`127.0.0.1:${port}`, { tls: false });
    try {
      const st = await statusOf(new ServiceClient(conn, "conformance.v1.NoteService", pool()).Sleep(new Context().withTimeout(100), msg("SleepRequest")()));
      expect(st.code).toBe(Status.DEADLINE_EXCEEDED);
      expect(await settled(() => seen.length === 3)).toBe(true);
      expect(seen).toEqual([
        "context deadline exceeded",
        Status.DEADLINE_EXCEEDED,
        "distx.run: not committing, the caller's context is done: context deadline exceeded",
      ]);
    } finally {
      conn.close();
      await stop(srv);
    }
  });

  it("an error detail and the operator's prefix", async () => {
    const st = await statusOf(stack.client.Refuse(new Context(), msg("RefuseRequest")()));
    expect(st.code).toBe(Status.FAILED_PRECONDITION);
    expect(st.message).toBe("NoteService.Refuse: locked by export");
    const d = errorDetail(st)!;
    expect([d.code, d.message, d.field]).toEqual(["NOTE_LOCKED", "This note is locked.", "text"]);
  });

  it("an unexpected error is INTERNAL and leaks nothing", async () => {
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    const st = await statusOf(stack.client.TxErrors(new Context(), msg("TxErrorsRequest")()));
    expect(st.code).toBe(Status.INTERNAL);
    expect(st.message).toBe("/conformance.v1.NoteService/TxErrors: internal error");
  });

  it("SDK errors answer UNKNOWN", async () => {
    const st = await statusOf(stack.client.Whoami(new Context(), msg("WhoamiRequest")()));
    expect(st.code).toBe(Status.UNKNOWN);
    expect(st.message).toContain("no authenticated caller");
  });

  it("emits after success, through self too", async () => {
    const note = await stack.client.CreateNote(new Context({ outgoing: { "w17-label-tenant": "t" } }), msg("CreateNoteRequest")({ text: "a" }));
    expect(stack.storage.rows).toEqual(["a"]);
    expect(stack.pub.published.length).toBe(1);
    const [ch, topic, raw] = stack.pub.published[0]!;
    expect([ch, topic]).toEqual(["notes", "note.created"]);
    const env = Event().fromBinary(raw);
    expect(env.payload.value.id).toBe(note.id);
    expect(env.payload.value.text).toBe("a");
    expect(env.labels).toEqual({ tenant: "t" });
  });

  it("nested emits wait for the root commit", async () => {
    await stack.client.CreateNotes(new Context(), msg("CreateNotesRequest")({ texts: ["x", "y"] }));
    expect(stack.storage.rows).toEqual(["x", "y"]);
    expect(stack.pub.published.map(([, , r]) => Event().fromBinary(r).payload.value.text)).toEqual(["x", "y"]);
    expect(stack.coord.calls).toEqual(["begin", "commit"]); // one transaction: the nested runs joined
  });

  it("a rolled-back transaction drops its events", async () => {
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    const st = await statusOf(stack.client.CreateNotes(new Context(), msg("CreateNotesRequest")({ texts: ["x"], fail: true })));
    expect(st.code).toBe(Status.INTERNAL);
    expect(stack.storage.rows).toEqual([]);
    expect(stack.pub.published).toEqual([]);
    expect(stack.coord.calls).toEqual(["begin", "rollback"]);
  });

  it("paging.forward reaches the one call", async () => {
    stack.storage.rows = ["a", "b", "c"];
    const resp = await stack.client.ListNotes(new Context({ outgoing: { [paging.LIMIT_MD_KEY]: "2" } }), msg("ListNotesRequest")());
    expect((resp.notes as AnyMessage[]).map((n) => n.text)).toEqual(["a", "b"]);
    const inspected = stack.storage.seen.filter(([n]) => n === "Inspect").at(-1)![1];
    expect(inspected.incoming.has(paging.LIMIT_MD_KEY)).toBe(false); // the sibling call did not get the page
  });

  it("paging.forward relays the page position", async () => {
    // The business method answers with the position the FORWARDED storage
    // call reported (so the gateway can build the next cursor without knowing
    // the sort), not the one its sibling Inspect call reported.
    stack.storage.rows = ["a", "b", "c"];
    const { header } = await stack.bconn.unary(
      new Context({ outgoing: { [paging.LIMIT_MD_KEY]: "2" } }),
      stack.client.methodInfo("ListNotes"),
      msg("ListNotesRequest")(),
    );
    expect(header.getAll(paging.NEXT_MD_KEY)).toEqual(["next-of-page"]);
    expect(header.getAll(paging.PREV_MD_KEY)).toEqual(["prev-of-page"]);
    expect(header.getAll(paging.TOTAL_MD_KEY)).toEqual(["3"]);
    // A method that forwards nothing answers no position.
    const plain = await stack.bconn.unary(new Context(), stack.client.methodInfo("Inspect"), msg("InspectRequest")());
    expect(plain.header.getAll(paging.NEXT_MD_KEY)).toEqual([]);
  });

  it("the client retries a trailers-only UNAVAILABLE, three attempts", async () => {
    // A server that answers UNAVAILABLE before any header, as a transport
    // refusal reads (no w17 handler ran).
    let refuse = 0;
    const info = new ServiceClient(stack.conn, "conformance.v1.NoteStorage", pool()).methodInfo("InsertNote");
    const raw = new Server();
    raw.addService({ InsertNote: methodDefinition(info) }, {
      InsertNote: (call: { request: AnyMessage }, cb: (e: Partial<ServiceError> | null, v?: unknown) => void) => {
        if (refuse > 0) {
          refuse--;
          cb({ code: Status.UNAVAILABLE, details: "try again" });
          return;
        }
        cb(null, msg("Note")({ text: call.request.text }));
      },
    });
    const port = await new Promise<number>((res, rej) =>
      raw.bindAsync("127.0.0.1:0", ServerCredentials.createInsecure(), (e, p) => (e ? rej(e) : res(p))),
    );
    const conn = new Conn(`127.0.0.1:${port}`, { tls: false });
    try {
      const storage = new ServiceClient(conn, "conformance.v1.NoteStorage", pool());
      refuse = 2;
      expect((await storage.InsertNote(new Context(), msg("InsertNoteRequest")({ text: "r" }))).text).toBe("r");
      refuse = 3;
      const st = await statusOf(storage.InsertNote(new Context(), msg("InsertNoteRequest")({ text: "r" })));
      expect(st.code).toBe(Status.UNAVAILABLE);
      expect(refuse).toBe(0); // exactly three attempts
    } finally {
      conn.close();
      await stop(raw);
    }
  });

  it("a handler's UNAVAILABLE is answered after headers and not replayed", async () => {
    const storage = new ServiceClient(stack.conn, "conformance.v1.NoteStorage", pool());
    stack.storage.handlerUnavailable = 1;
    const st = await statusOf(storage.InsertNote(new Context(), msg("InsertNoteRequest")({ text: "once" })));
    expect(st.code).toBe(Status.UNAVAILABLE);
    expect(stack.storage.rows).toEqual(["once"]); // one write, no replay
    // On the wire: response headers carrying w17-answered, then the status.
    const m = storage.methodInfo("InsertNote");
    const c = new Client(stack.conn.addr, credentials.createInsecure());
    try {
      stack.storage.handlerUnavailable = 1;
      const md = await new Promise<GrpcMetadata>((res) => {
        const call = c.makeUnaryRequest(m.path, serializer(m.input) as (v: unknown) => Buffer, (b: Buffer) => b,
          msg("InsertNoteRequest")({ text: "raw" }), new GrpcMetadata(), () => {});
        call.on("metadata", res);
      });
      expect(md.get("w17-answered")).toEqual(["1"]);
    } finally {
      c.close();
    }
  });

  it("the client refuses a done context", async () => {
    const storage = new ServiceClient(stack.conn, "conformance.v1.NoteStorage", pool());
    const ctx = new Context().withCancel();
    ctx.cancel();
    const before = stack.storage.seen.length;
    const st = await statusOf(storage.Inspect(ctx, msg("InspectRequest")()));
    expect(st.code).toBe(Status.CANCELLED);
    expect(stack.storage.seen.length).toBe(before);
  });

  it("cancelling the context cancels the call", async () => {
    const ctx = new Context().withCancel();
    const call = statusOf(stack.client.Sleep(ctx, msg("SleepRequest")({ ms: 3000 })));
    await sleep(50);
    ctx.cancel();
    expect((await call).code).toBe(Status.CANCELLED);
  });

  it("unreachable storage is UNAVAILABLE", async () => {
    const conn = new Conn("127.0.0.1:1", { tls: false });
    try {
      const st = await statusOf(new ServiceClient(conn, "conformance.v1.NoteStorage", pool()).Inspect(new Context(), msg("InspectRequest")()));
      expect(st.code).toBe(Status.UNAVAILABLE);
    } finally {
      conn.close();
    }
  });

  it("draining refuses new calls", async () => {
    stack.registry.draining = true;
    try {
      expect((await statusOf(stack.client.Inspect(new Context(), msg("InspectRequest")()))).code).toBe(Status.UNAVAILABLE);
    } finally {
      stack.registry.draining = false;
    }
  });

  it("a plain grpc-js client reads the status and the details", async () => {
    const m = stack.client.methodInfo("Refuse");
    const c = new Client(stack.bconn.addr, credentials.createInsecure());
    try {
      const err = await new Promise<ServiceError>((res) =>
        c.makeUnaryRequest(m.path, (b: Buffer) => b, (b: Buffer) => b, Buffer.alloc(0), new GrpcMetadata(), (e) => res(e!)),
      );
      expect(err.code).toBe(Status.FAILED_PRECONDITION);
      expect(err.metadata.get("grpc-status-details-bin").length).toBe(1);
    } finally {
      c.close();
    }
  });
});

// --- registration ---------------------------------------------------------------

describe("registration", () => {
  it("a missing method stops it", () => {
    class Partial {
      async CreateNote() {}
    }
    expect(() => new Registry(pool()).register("conformance.v1.NoteService", new Partial())).toThrow(/missing method.*CreateNotes/);
  });

  it("camelCase is not the RPC name", () => {
    const ok = new Registry(pool());
    ok.register("conformance.v1.NoteService", new NoteService(ok, undefined, undefined)); // exact names present: fine
    class OnlyCamel {
      async createNote() {}
    }
    expect(() => new Registry(pool()).register("conformance.v1.NoteService", new OnlyCamel())).toThrow(/CreateNote/);
  });

  it("a method that is not async is refused", () => {
    class Sync extends NoteService {
      // @ts-expect-error -- deliberately not async
      override CreateNote() {
        return undefined;
      }
    }
    const reg = new Registry(pool());
    expect(() => reg.register("conformance.v1.NoteService", new Sync(reg, undefined, undefined))).toThrow(/CreateNote.*an async function/);
  });

  it("an unknown or duplicate service", () => {
    const reg = new Registry(pool());
    expect(() => reg.register("nope.Service", {})).toThrow(/no service/);
    reg.register("conformance.v1.NoteService", new NoteService(reg, undefined, undefined));
    expect(() => reg.register("conformance.v1.NoteService", new NoteService(reg, undefined, undefined))).toThrow(/already registered/);
    expect(() => reg.self("conformance.v1.NoteStorage")).toThrow(/not registered/);
    expect(() => new Registry(pool(), { services: [] }).register("NoteService", new NoteService(reg, undefined, undefined))).toThrow(
      RegistrationError,
    );
  });

  it("a direct call skips the emit; self does not", async () => {
    class Local extends NoteService {
      override async CreateNote(_ctx: Context, req: AnyMessage) {
        return msg("Note")({ id: 1n, text: req.text });
      }
    }
    const pub = new events.MemoryPublisher();
    const reg = new Registry(pool(), { emits: [EMIT], dispatcher: pub });
    const impl = new Local(reg, undefined, undefined);
    reg.register("conformance.v1.NoteService", impl);
    await impl.CreateNote(new Context(), msg("CreateNoteRequest")({ text: "direct" }));
    expect(pub.published).toEqual([]);
    await reg.self("NoteService").CreateNote(new Context(), msg("CreateNoteRequest")({ text: "served" }));
    expect(pub.published.length).toBe(1);
  });
});

// --- streams: a service built for the test ------------------------------------

function streamPool(p: Pool): void {
  const Ping = { name: "Ping", field: [{ name: "n", number: 1, label: 1, type: 5, jsonName: "n" }] };
  const m = (name: string, cs: boolean, ss: boolean) => ({
    name, inputType: ".test.stream.Ping", outputType: ".test.stream.Ping", clientStreaming: cs, serverStreaming: ss,
  });
  const file = create(FileDescriptorProtoSchema, {
    name: "test/stream.proto",
    package: "test.stream",
    syntax: "proto3",
    messageType: [Ping],
    service: [{ name: "Streamer", method: [m("Echo", false, false), m("Count", false, true), m("Sum", true, false), m("Chat", true, true)] }],
  });
  p.addSet(toBinary(FileDescriptorSetSchema, create(FileDescriptorSetSchema, { file: [file] })));
}

class Streamer {
  async Echo(_ctx: Context, req: AnyMessage) {
    return pool().message("test.stream.Ping")({ n: req.n });
  }
  async *Count(_ctx: Context, req: AnyMessage) {
    for (let i = 1; i <= req.n; i++) yield pool().message("test.stream.Ping")({ n: i });
  }
  async Sum(_ctx: Context, reqs: AsyncIterable<AnyMessage>) {
    let n = 0;
    for await (const r of reqs) n += r.n as number;
    return pool().message("test.stream.Ping")({ n });
  }
  async *Chat(_ctx: Context, reqs: AsyncIterable<AnyMessage>) {
    for await (const r of reqs) yield pool().message("test.stream.Ping")({ n: (r.n as number) * 10 });
  }
}

describe("streams", () => {
  it("every kind is served, and none can emit", async () => {
    const p = pool();
    streamPool(p);
    const Ping = p.message("test.stream.Ping");
    const reg = new Registry(p);
    reg.register("test.stream.Streamer", new Streamer());
    const [srv, port] = await listen(reg);
    const conn = new Conn(`127.0.0.1:${port}`, { tls: false });
    try {
      const c = new ServiceClient(conn, "test.stream.Streamer", p);
      expect((await c.Echo(new Context(), Ping({ n: 4 }))).n).toBe(4);
      const counted: number[] = [];
      for await (const r of c.Count(new Context(), Ping({ n: 3 }))) counted.push(r.n);
      expect(counted).toEqual([1, 2, 3]);
      expect((await c.Sum(new Context(), [Ping({ n: 1 }), Ping({ n: 2 }), Ping({ n: 3 })])).n).toBe(6);
      const chat: number[] = [];
      for await (const r of c.Chat(new Context(), [Ping({ n: 1 }), Ping({ n: 2 })])) chat.push(r.n);
      expect(chat).toEqual([10, 20]);
    } finally {
      conn.close();
      await stop(srv);
    }
    const bad: Emit = { ...EMIT, method: "/test.stream.Streamer/Count", event: "test.stream.Ping", fields: [] };
    expect(() => new Registry(p, { emits: [bad] }).register("test.stream.Streamer", new Streamer())).toThrow(/streaming/);
    class NotAGenerator extends Streamer {
      // @ts-expect-error -- deliberately an async function, not an async generator
      override async Count() {
        return undefined;
      }
    }
    expect(() => new Registry(p).register("test.stream.Streamer", new NotAGenerator())).toThrow(/Count.*async generator/);
  });

  it("a request stream that fails once the caller is gone reads as the cancel, not a panic", async () => {
    const logged: string[] = [];
    vi.spyOn(console, "error").mockImplementation((...a: unknown[]) => void logged.push(a.join(" ")));
    const read = async (ctx: Context, fail: () => void) => {
      // What grpc-js's request stream does when the client resets it mid-read.
      const call = {
        async *iterator() {
          yield 1;
          fail();
          throw new Error("premature close");
        },
      };
      const got: unknown[] = [];
      try {
        for await (const r of requests(call as never, ctx)) got.push(r);
      } catch (e) {
        return [got, e] as const;
      }
      throw new Error("the stream did not fail");
    };
    const ctx = new Context().withCancel();
    const [got, err] = await read(ctx, () => ctx.cancel());
    expect(got).toEqual([1]);
    expect(err).toBeInstanceOf(StatusError);
    expect([(err as StatusError).code, (err as StatusError).message]).toEqual([Status.CANCELLED, "context canceled"]);
    // The handler rethrows it: answered as it is, no panic reported.
    expect(toServiceError(err, "/test.stream.Streamer/Sum", ctx).code).toBe(Status.CANCELLED);
    expect(logged.filter((l) => l.includes("PANIC"))).toEqual([]);
    // A failure while the caller is still there stays what it is.
    const [, other] = await read(new Context(), () => undefined);
    expect((other as Error).message).toBe("premature close");
  });

  it("a deadline that passes mid-stream ends the request stream as the deadline, not as its end", async () => {
    const logged: string[] = [];
    vi.spyOn(console, "error").mockImplementation((...a: unknown[]) => void logged.push(a.join(" ")));
    const p = pool();
    streamPool(p);
    const Ping = p.message("test.stream.Ping");
    const seen: unknown[] = [];
    let got = 0;
    class Reading extends Streamer {
      override async Sum(_ctx: Context, reqs: AsyncIterable<AnyMessage>) {
        try {
          for await (const _r of reqs) got++;
          seen.push("ended"); // a truncated upload taken as complete
        } catch (e) {
          seen.push(e);
          throw e;
        }
        return Ping({ n: got });
      }
    }
    const reg = new Registry(p);
    reg.register("test.stream.Streamer", new Reading());
    const [srv, port] = await listen(reg);
    const conn = new Conn(`127.0.0.1:${port}`, { tls: false });
    try {
      async function* sends() {
        yield Ping({ n: 1 });
        await sleep(2000); // the stream stays open past the deadline
      }
      const st = await statusOf(new ServiceClient(conn, "test.stream.Streamer", p).Sum(new Context().withTimeout(200), sends()));
      expect(st.code).toBe(Status.DEADLINE_EXCEEDED);
      expect(await settled(() => seen.length === 1 && reg.inflight === 0)).toBe(true);
      expect(seen[0]).toBeInstanceOf(StatusError);
      expect((seen[0] as StatusError).code).toBe(Status.DEADLINE_EXCEEDED);
      expect(logged.filter((l) => l.includes("PANIC"))).toEqual([]);
    } finally {
      conn.close();
      await stop(srv);
    }
  });

  it("a handler that throws something unprintable still answers INTERNAL, traced or not", async () => {
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    const p = pool();
    streamPool(p);
    const Ping = p.message("test.stream.Ping");
    class Throws extends Streamer {
      override async Echo(_ctx: Context, req: AnyMessage): Promise<AnyMessage> {
        // String() of the first throws; JSON of the second (the span status) does.
        throw req.n === 1 ? Object.create(null) : { n: 1n };
      }
    }
    const reg = new Registry(p);
    reg.register("test.stream.Streamer", new Throws());
    const [srv, port] = await listen(reg);
    const conn = new Conn(`127.0.0.1:${port}`, { tls: false });
    const unhandled: unknown[] = [];
    const onUnhandled = (e: unknown) => void unhandled.push(e);
    process.on("unhandledRejection", onUnhandled);
    const obs = await observability.init("svc", { environ: { W17_OTEL_ENDPOINT: "127.0.0.1:1" } });
    try {
      const c = new ServiceClient(conn, "test.stream.Streamer", p);
      for (const n of [1, 2]) {
        const st = await statusOf(c.Echo(new Context().withTimeout(2000), Ping({ n })));
        expect([n, st.code]).toEqual([n, Status.INTERNAL]);
      }
      await sleep(20);
      expect(unhandled).toEqual([]);
    } finally {
      process.off("unhandledRejection", onUnhandled);
      await obs.shutdown(100);
      conn.close();
      srv.forceShutdown();
    }
  });

  it("a handler that stops reading the requests still answers, as Go's that stops calling Recv", async () => {
    const p = pool();
    streamPool(p);
    const Ping = p.message("test.stream.Ping");
    class StopsReading extends Streamer {
      override async Sum(_ctx: Context, reqs: AsyncIterable<AnyMessage>) {
        for await (const r of reqs) return Ping({ n: (r.n as number) + 100 });
        return Ping({ n: -1 });
      }
      override async *Chat(_ctx: Context, reqs: AsyncIterable<AnyMessage>) {
        let first = 0;
        for await (const r of reqs) {
          first = r.n as number;
          break;
        }
        for (let i = 1; i <= 3; i++) yield Ping({ n: first * 10 + i });
      }
    }
    const reg = new Registry(p);
    reg.register("test.stream.Streamer", new StopsReading());
    const [srv, port] = await listen(reg);
    // A plain grpc-js client: every request written and the stream
    // half-closed up front, so only the server decides how the call ends.
    const client = new Client(`127.0.0.1:${port}`, credentials.createInsecure());
    const ser = (m: AnyMessage) => Buffer.from(Ping.toBinary(m));
    const de = (b: Buffer) => Ping.fromBinary(b);
    const N = 2000;
    try {
      const deadline = new Date(Date.now() + 3000);
      const chat = client.makeBidiStreamRequest("/test.stream.Streamer/Chat", ser, de, new GrpcMetadata(), { deadline });
      const replies: number[] = [];
      const status = new Promise<number>((res) => chat.on("status", (s: { code: number }) => res(s.code)));
      chat.on("data", (r: AnyMessage) => replies.push(r.n as number));
      chat.on("error", () => undefined);
      for (let i = 1; i <= N; i++) chat.write(Ping({ n: i }));
      chat.end();
      const t0 = Date.now();
      expect(await status).toBe(Status.OK);
      expect(Date.now() - t0).toBeLessThan(2000);
      expect(replies).toEqual([11, 12, 13]);

      const sum = await new Promise<number>((res, rej) => {
        const call = client.makeClientStreamRequest("/test.stream.Streamer/Sum", ser, de, new GrpcMetadata(), { deadline }, (e, r) =>
          e ? rej(e) : res((r as AnyMessage).n as number),
        );
        for (let i = 1; i <= N; i++) call.write(Ping({ n: i }));
        call.end();
      });
      expect(sum).toBe(101);
      expect(await settled(() => reg.inflight === 0)).toBe(true);
    } finally {
      client.close();
      srv.forceShutdown(); // a hung call must not hang the suite too
    }
  });

  it("a server that answers before the caller's requests run out ends the call; the request iterable is closed", async () => {
    const p = pool();
    streamPool(p);
    const Ping = p.message("test.stream.Ping");
    class AnswersEarly extends Streamer {
      override async Sum(_ctx: Context, reqs: AsyncIterable<AnyMessage>) {
        for await (const r of reqs) return Ping({ n: r.n });
        return Ping({ n: -1 });
      }
      override async *Chat(_ctx: Context, reqs: AsyncIterable<AnyMessage>) {
        for await (const r of reqs) {
          yield Ping({ n: r.n });
          return;
        }
      }
    }
    const reg = new Registry(p);
    reg.register("test.stream.Streamer", new AnswersEarly());
    const [srv, port] = await listen(reg);
    const conn = new Conn(`127.0.0.1:${port}`, { tls: false });
    try {
      const c = new ServiceClient(conn, "test.stream.Streamer", p);
      // An endless request stream, as a caller that streams until told to stop.
      const endless = (closed: string[], name: string) =>
        (async function* () {
          try {
            for (let i = 1; ; i++) {
              yield Ping({ n: i });
              await sleep(5);
            }
          } finally {
            closed.push(name);
          }
        })();
      const closed: string[] = [];
      const within = <T>(pr: Promise<T>, what: string) =>
        Promise.race([pr, sleep(3000).then(() => Promise.reject(new Error(`${what}: still waiting after 3s`)))]);

      const got: number[] = [];
      await within(
        (async () => {
          for await (const r of c.Chat(new Context(), endless(closed, "chat"))) got.push(r.n);
        })(),
        "bidi",
      );
      expect(got).toEqual([1]);
      expect(await settled(() => closed.includes("chat"))).toBe(true);

      expect((await within<AnyMessage>(c.Sum(new Context(), endless(closed, "sum")), "client stream")).n).toBe(1);
      expect(await settled(() => closed.includes("sum"))).toBe(true);
      expect(await settled(() => reg.inflight === 0)).toBe(true);
    } finally {
      conn.close();
      srv.forceShutdown();
    }
  });
});

// --- registry.self is Go's direct call: the callee gets the caller's ctx ------

const envOf = (userId: string) => Buffer.from(msg("AuthResp").toBinary(msg("AuthResp")({ userId }))).toString("base64");

describe("registry.self", () => {
  it("hands the callee the caller's context unchanged", async () => {
    // Go parity: s.self.Method(ctx, req) is a direct call. The callee sees the
    // caller's incoming metadata, whatever the caller put on its outgoing
    // side, and the open transaction, which its own distx.begin JOINS.
    principal.setAuthRespType(msg("AuthResp"));
    const seen: Record<string, unknown> = {};
    const calls: string[] = [];
    const tx: distx.TxClient = {
      begin: async () => {
        calls.push("begin");
        return { reply: DM("BeginResponse", { txId: "tx-9" }), header: new Metadata() };
      },
      commit: async () => undefined,
      rollback: async () => undefined,
    };
    class Local extends NoteService {
      override async Inspect(ctx: Context) {
        seen.incoming = ctx.incoming.toObject();
        seen.outgoing = ctx.outgoing.toObject();
        seen.user = principal.userId(ctx);
        const [h, joined] = await distx.begin(ctx, tx);
        seen.joined = h.joined;
        seen.tx = joined.outgoing.getAll(distx.TX_ID_HEADER);
        return msg("InspectResponse")();
      }
    }
    const reg = new Registry(pool());
    reg.register("conformance.v1.NoteService", new Local(reg, undefined, undefined));
    const incoming = { "x-w17-user": envOf("person"), "w17-language": "cs", "x-custom": "c" };
    const [, txCtx] = await distx.begin(new Context({ incoming }), tx);
    const caller = txCtx.withOutgoing(txCtx.outgoing.withSet("x-mine", "m"));
    await reg.self("NoteService").Inspect(caller, msg("InspectRequest")());
    expect(seen.incoming).toEqual(new Metadata(incoming).toObject());
    expect(seen.outgoing).toEqual({ [distx.TX_ID_HEADER]: ["tx-9"], "x-mine": ["m"] });
    expect(seen.user).toBe("person");
    expect([seen.joined, seen.tx]).toEqual([true, ["tx-9"]]);
    expect(calls).toEqual(["begin"]); // the callee joined; it began nothing
  });

  it("actAs on a ctx given to self is not the callee's principal", async () => {
    // As in Go: actAs sets the OUTGOING side (and strips the incoming one),
    // and the callee's readers read INCOMING — so a direct call does not see it.
    principal.setAuthRespType(msg("AuthResp"));
    let user: string | undefined = "unset";
    class Local extends NoteService {
      override async Whoami(ctx: Context) {
        try {
          user = principal.userId(ctx);
        } catch (e) {
          if (!(e instanceof principal.NoPrincipalError)) throw e;
          user = undefined;
        }
        return msg("WhoamiResponse")();
      }
    }
    const reg = new Registry(pool());
    reg.register("conformance.v1.NoteService", new Local(reg, undefined, undefined));
    const caller = new Context({ incoming: { "x-w17-user": envOf("person") } });
    const acted = principal.actAs(caller, msg("AuthResp")({ userId: "robot" }));
    await reg.self("NoteService").Whoami(acted, msg("WhoamiRequest")());
    expect(user).toBeUndefined();
  });
});

// --- an emit outlives the request that caused it ------------------------------

describe("emits after the request", () => {
  class Local extends NoteService {
    override async CreateNote(_ctx: Context, req: AnyMessage) {
      return msg("Note")({ id: 1n, text: req.text });
    }
  }

  it("an emit outside a transaction outlives a caller that stopped waiting", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let started = false;
    const pub = new events.MemoryPublisher();
    const slow: events.Dispatcher = {
      dispatch: async (ctx, ch, t, p) => {
        started = true;
        await gate;
        await pub.dispatch(ctx, ch, t, p);
      },
      close: async () => {},
      publishTimeoutMs: 5000,
    };
    const reg = new Registry(pool(), { emits: [EMIT], dispatcher: slow });
    reg.register("conformance.v1.NoteService", new Local(reg, undefined, undefined));
    const ctx = new Context().withCancel();
    const call = reg.self("NoteService").CreateNote(ctx, msg("CreateNoteRequest")({ text: "x" }));
    expect(await settled(() => started)).toBe(true);
    ctx.cancel(); // the client hung up
    expect(txscope.pendingCount()).toBeGreaterThan(0); // the drain would wait for it
    release();
    await call;
    expect(pub.published.length).toBe(1); // the committed write's event was not lost with the request
  });

  it("a hanging broker is bounded by the publish timeout", async () => {
    const reported: string[] = [];
    vi.spyOn(console, "error").mockImplementation((...a: unknown[]) => void reported.push(a.join(" ")));
    const old = events.settings.publishTimeoutMs;
    events.settings.publishTimeoutMs = 50;
    try {
      const hanging: events.Dispatcher = { dispatch: () => new Promise(() => undefined), close: async () => {} };
      const reg = new Registry(pool(), { emits: [EMIT], dispatcher: hanging });
      reg.register("conformance.v1.NoteService", new Local(reg, undefined, undefined));
      const note = await reg.self("NoteService").CreateNote(new Context(), msg("CreateNoteRequest")({ text: "x" }));
      expect(note.text).toBe("x");
      expect(reported.some((r) => r.includes("note.created"))).toBe(true);
    } finally {
      events.settings.publishTimeoutMs = old;
    }
  });
});
