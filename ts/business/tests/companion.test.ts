// The companion's SDK half: manifest format 2, the companion connection (no
// retries, no transaction or paging headers, the principal relayed), the
// egress clients and the file stores — over a real grpc-js server standing in
// for the Go companion (the conformance suite runs the real one).

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { BinaryWriter, WireType } from "@bufbuild/protobuf/wire";
import { Metadata as GrpcMetadata, Server, ServerCredentials } from "@grpc/grpc-js";
import { afterAll, beforeAll, beforeEach, describe, expect, it } from "vitest";
import { CompanionConn, stripCompanionMetadata } from "../src/client.ts";
import { Context } from "../src/context.ts";
import type { AnyMessage } from "../src/descriptors.ts";
import * as egress from "../src/egress.ts";
import { Status, StatusError, encodeRpcStatus, errorInfo } from "../src/errors.ts";
import * as files from "../src/files.ts";
import { ManifestError, fromObject, load, parseJSON } from "../src/manifest.ts";
import { BootError, Clients } from "../src/runtime.ts";
import { Registry } from "../src/server.ts";
import { MANIFEST, pool } from "./helpers.ts";

type Obj = Record<string, any>;

const conformance = (): Obj => parseJSON(readFileSync(MANIFEST, "utf8")) as Obj;

function errorInfoBytes(domain: string, reason: string, meta: Record<string, string> = {}): Uint8Array {
  const w = new BinaryWriter();
  w.tag(1, WireType.LengthDelimited).string(reason);
  w.tag(2, WireType.LengthDelimited).string(domain);
  for (const [k, v] of Object.entries(meta)) {
    w.tag(3, WireType.LengthDelimited).fork();
    w.tag(1, WireType.LengthDelimited).string(k);
    w.tag(2, WireType.LengthDelimited).string(v);
    w.join();
  }
  return w.finish();
}

function withInfo(code: Status, message: string, domain: string, reason: string, meta: Record<string, string> = {}): StatusError {
  const details = encodeRpcStatus({
    code,
    message,
    details: [{ typeUrl: "type.googleapis.com/google.rpc.ErrorInfo", value: errorInfoBytes(domain, reason, meta) }],
  });
  return new StatusError(code, message, details);
}

describe("manifest format 2", () => {
  it("reads the companion section", () => {
    const m = load(MANIFEST);
    const c = m.companion!;
    expect(m.format).toBe(2);
    expect([c.addressEnv, c.defaultAddress]).toEqual(["NOTES_COMPANION_ADDR", "notes-business-companion:50051"]);
    expect(c.egress).toEqual([{ client: "echo", service: "clients.echo.EchoApi", dsnEnv: "W17_CLIENT_ECHO_DSN" }]);
    expect(c.files!.service).toBe(files.SERVICE);
    expect(c.files!.connections.map((x) => [x.name, x.writable])).toEqual([["notes-files", true], ["notes-media", false]]);
    expect(c.files!.placements).toEqual([
      {
        field: "conformance.v1.NoteFile.file",
        name: "NoteFileFile",
        connection: "notes-files",
        bucketPath: "/note-files",
        expectedObjects: 100000n,
        maxPerBucket: 100,
        maxSizeBytes: 262144n,
      },
    ]);
    expect("companion" in m.extra).toBe(false);
  });

  it("reads a 64-bit count exactly", () => {
    const src = readFileSync(MANIFEST, "utf8").replace('"expected_objects": 100000', '"expected_objects": 18446744073709551615');
    const m = fromObject(parseJSON(src) as Obj);
    expect(m.companion!.files!.placements[0]!.expectedObjects).toBe(18446744073709551615n);
  });

  it("refuses what it does not read", () => {
    let d = conformance();
    d.format = 3;
    expect(() => fromObject(d)).toThrow(/reads formats 1 and 2/);
    d.format = 1;
    expect(() => fromObject(d)).toThrow(/format 1 has no companion section/);
    d.companion = null;
    expect(fromObject(d).companion).toBeNull();
    d = conformance();
    d.companion.files.placements[0].connection = "nope";
    expect(() => fromObject(d)).toThrow(/not in companion.files.connections/);
    d = conformance();
    delete d.companion.egress[0].dsn_env;
    expect(() => fromObject(d)).toThrow(ManifestError);
  });

  it("refuses what the Go companion refuses", () => {
    const refused = (edit: (c: Obj) => void, want: RegExp) => {
      const d = conformance();
      edit(d.companion);
      expect(() => fromObject(d)).toThrow(want);
    };
    refused((c) => {
      for (const k of Object.keys(c)) delete c[k];
    }, /companion\.address_env is required/);
    refused((c) => {
      c.address_env = "";
    }, /companion\.address_env is required/);
    refused((c) => {
      c.files.placements.push({ ...c.files.placements[0] });
    }, /companion\.files\.placements: conformance\.v1\.NoteFile\.file is listed twice/);
    refused((c) => {
      c.egress.push({ ...c.egress[0] });
    }, /companion\.egress: client echo is listed twice/);
    refused((c) => {
      c.egress.push({ ...c.egress[0], client: "echo2", dsn_env: "W17_CLIENT_ECHO2_DSN" });
    }, /companion\.egress: service clients\.echo\.EchoApi is listed twice/);
    for (const bad of ["false", "true", 0, 1, []]) {
      refused((c) => {
        c.files.connections[0].writable = bad;
      }, /companion\.files\.connections\[\]\.writable must be true or false/);
    }
    // Absent or null is false; a JSON bool is read as it is.
    const d = conformance();
    d.companion.files.connections[0].writable = null;
    delete d.companion.files.connections[1].writable;
    expect(fromObject(d).companion!.files!.connections.map((c) => c.writable)).toEqual([false, false]);
  });
});

describe("ErrorInfo", () => {
  it("is decoded by hand from the status details", () => {
    const e = withInfo(Status.NOT_FOUND, "echo: upstream answered HTTP 404", "echo", "UPSTREAM_HTTP_404", { http_status: "404", attempts: "1" });
    expect(egress.errorInfo(e)).toEqual({ domain: "echo", reason: "UPSTREAM_HTTP_404", metadata: { http_status: "404", attempts: "1" } });
    expect(errorInfo(new StatusError(Status.NOT_FOUND, "no details"))).toBeUndefined();
    expect(errorInfo(new Error("not a status"))).toBeUndefined();
    expect(errorInfo(undefined)).toBeUndefined();
    const broken = encodeRpcStatus({ code: 13, message: "x", details: [{ typeUrl: "type.googleapis.com/google.rpc.ErrorInfo", value: new Uint8Array([0x0a, 0xff]) }] });
    expect(errorInfo(new StatusError(Status.INTERNAL, "x", broken))).toBeUndefined();
  });

  it("isNotFound is the files reason only", () => {
    expect(files.isNotFound(withInfo(Status.NOT_FOUND, "x", files.DOMAIN, "FILE_NOT_FOUND", { connection: "c" }))).toBe(true);
    expect(files.isNotFound(withInfo(Status.NOT_FOUND, "x", "echo", "UPSTREAM_HTTP_404"))).toBe(false);
    expect(files.isNotFound(new StatusError(Status.NOT_FOUND, "x"))).toBe(false);
    expect(files.isNotFound(new RangeError("x"))).toBe(false);
  });
});

it("the companion metadata loses the transaction and paging headers", () => {
  const md = new GrpcMetadata();
  for (const [k, v] of [
    ["x-w17-user", "u"],
    ["w17-tx-id", "t"],
    ["w17-conn-id", "c"],
    ["x-w17-paging-limit", "2"],
    ["w17-language", "cs"],
  ] as const) md.add(k, v);
  md.add("x-w17-paging-boundaries-bin", Buffer.from("b"));
  expect(Object.keys(stripCompanionMetadata(md).getMap()).sort()).toEqual(["w17-language", "x-w17-user"]);
});

/** The Go companion's behaviour, as far as the SDK can see it. */
class FakeCompanion {
  calls: [string, Context][] = [];
  objects = new Map<string, Uint8Array>();
  putChunks: number[] = [];
  /** The size of every end marker a Put sent. */
  putEnds: bigint[] = [];
  unavailable = 0;
  n = 0;
  M = (name: string, init: Record<string, unknown> = {}) => pool().message(name)(init);

  async GetEcho(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    this.calls.push(["GetEcho", ctx]);
    if (this.unavailable) {
      this.unavailable--;
      throw withInfo(Status.UNAVAILABLE, "echo: upstream answered HTTP 503", "echo", "UPSTREAM_HTTP_503");
    }
    if (req.id === "missing") throw withInfo(Status.NOT_FOUND, "echo: upstream answered HTTP 404", "echo", "UPSTREAM_HTTP_404", { http_status: "404" });
    return this.M("clients.echo.Echo", { id: req.id, message: `echo ${req.id}` });
  }

  #notFound(conn: string): StatusError {
    return withInfo(Status.NOT_FOUND, "w17.files: no stored file", files.DOMAIN, "FILE_NOT_FOUND", { connection: conn });
  }

  async Put(ctx: Context, reqs: AsyncIterable<AnyMessage>): Promise<AnyMessage> {
    this.calls.push(["Put", ctx]);
    let field = "";
    let ended = false;
    const parts: Uint8Array[] = [];
    for await (const r of reqs) {
      if (r.part.case === "header") field = r.part.value.field;
      else if (r.part.case === "end") {
        this.putEnds.push(r.part.value.size);
        ended = r.part.value.size === BigInt(Buffer.concat(parts).length);
      } else {
        this.putChunks.push(r.part.value.length);
        parts.push(r.part.value);
      }
    }
    const body = Buffer.concat(parts);
    if (body.length > 262144) {
      throw withInfo(Status.INVALID_ARGUMENT, "w17.files: too large", files.DOMAIN, "FILE_TOO_LARGE", { field, limit_bytes: "262144" });
    }
    // As kvfsgrpc: only the end marker commits.
    if (!ended) throw withInfo(Status.INVALID_ARGUMENT, "w17.files: no end marker", files.DOMAIN, "BAD_REQUEST");
    this.n++;
    const handle = `note-files/${createHash("sha256").update(body).digest("hex")}-${this.n.toString(16).padStart(32, "0")}`;
    this.objects.set(handle, body);
    return this.M("w17.common.files.PutResponse", { handle });
  }

  async *Open(ctx: Context, req: AnyMessage): AsyncGenerator<AnyMessage> {
    this.calls.push(["Open", ctx]);
    const body = this.objects.get(req.handle);
    if (body === undefined) throw this.#notFound(req.connection);
    for (let i = 0; i < body.length; i += 64 << 10) yield this.M("w17.common.files.OpenResponse", { chunk: body.subarray(i, i + (64 << 10)) });
  }

  async Stat(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    this.calls.push(["Stat", ctx]);
    const body = this.objects.get(req.handle);
    if (body === undefined) throw this.#notFound(req.connection);
    return this.M("w17.common.files.StatResponse", { size: BigInt(body.length), modTime: { seconds: 1_700_000_000n, nanos: 123_456_789 } });
  }

  async Delete(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    this.calls.push(["Delete", ctx]);
    this.objects.delete(req.handle);
    return this.M("w17.common.files.DeleteResponse");
  }
}

describe("over the wire", () => {
  let fake: FakeCompanion;
  let clients: Clients;
  let server: Server;

  beforeAll(async () => {
    const p = pool();
    fake = new FakeCompanion();
    const reg = new Registry(p);
    reg.register("clients.echo.EchoApi", fake);
    reg.register(files.SERVICE, fake);
    server = new Server();
    reg.addTo(server);
    const port = await new Promise<number>((res, rej) =>
      server.bindAsync("127.0.0.1:0", ServerCredentials.createInsecure(), (e, n) => (e ? rej(e) : res(n))),
    );
    clients = new Clients(p, load(MANIFEST), { NOTES_COMPANION_ADDR: `127.0.0.1:${port}`, NOTES_STORAGE_BACKEND_ADDR: "127.0.0.1:1" });
  });

  afterAll(async () => {
    clients.close();
    await new Promise<void>((r) => server.tryShutdown(() => r()));
  });

  beforeEach(() => {
    fake.calls = [];
    fake.putChunks = [];
  });

  const principalB64 = () => Buffer.from(pool().message("conformance.v1.AuthResp").toBinary(pool().message("conformance.v1.AuthResp")({ userId: "u-1" }))).toString("base64");
  const getEcho = (id: string) => pool().message("clients.echo.GetEchoRequest")({ id });

  it("an egress call relays the principal and nothing transactional", async () => {
    const ctx = new Context({
      incoming: { "x-w17-user": principalB64(), "w17-label-tenant": "t-1" },
      outgoing: { "w17-tx-id": "tx-9", "w17-conn-id": "c-9", "x-w17-paging-limit": "5", "w17-language": "cs" },
    }).withTimeout(5000);
    const echo = clients.egress("echo");
    expect(clients.egress("clients.echo.EchoApi")).toBe(echo);
    const reply = await echo.GetEcho(ctx, getEcho("42"));
    expect(reply.message).toBe("echo 42");
    const [method, seen] = fake.calls.at(-1)!;
    expect(method).toBe("GetEcho");
    expect(seen.incoming.first("x-w17-user")).toBe(principalB64());
    expect(seen.incoming.first("w17-label-tenant")).toBe("t-1");
    expect(seen.incoming.first("w17-language")).toBe("cs");
    for (const k of ["w17-tx-id", "w17-conn-id", "x-w17-paging-limit"]) expect(seen.incoming.first(k)).toBeUndefined();
    const rem = seen.timeRemaining();
    expect(rem !== undefined && rem > 0 && rem <= 5000).toBe(true);
  });

  it("egress errors pass through with their ErrorInfo", async () => {
    const e = await clients.egress("echo").GetEcho(new Context(), getEcho("missing")).catch((x: unknown) => x);
    expect(e).toBeInstanceOf(StatusError);
    expect((e as StatusError).code).toBe(Status.NOT_FOUND);
    expect(egress.errorInfo(e)).toEqual({ domain: "echo", reason: "UPSTREAM_HTTP_404", metadata: { http_status: "404" } });
  });

  it("the companion connection does not retry", async () => {
    fake.unavailable = 1;
    const e = await clients.egress("echo").GetEcho(new Context(), getEcho("1")).catch((x: unknown) => x);
    expect((e as StatusError).code).toBe(Status.UNAVAILABLE);
    expect(fake.calls.map((c) => c[0])).toEqual(["GetEcho"]);
  });

  it("a done context is refused before sending", async () => {
    const e = await clients.egress("echo").GetEcho(new Context().withTimeout(0), getEcho("1")).catch((x: unknown) => x);
    expect((e as StatusError).code).toBe(Status.DEADLINE_EXCEEDED);
    expect(fake.calls).toEqual([]);
  });

  it("unknown names are refused naming the known ones", () => {
    expect(() => clients.egress("nope")).toThrow(/have: echo/);
    expect(() => clients.store("nope")).toThrow(/have: notes-files, notes-media/);
  });

  it("a file round trip", async () => {
    const store = clients.store("notes-files");
    expect([store.name, store.writable]).toEqual(["notes-files", true]);
    const body = new Uint8Array(200 << 10).map((_, i) => i % 251);
    const ctx = new Context({ outgoing: { "w17-tx-id": "tx-1" } });
    fake.putEnds = [];
    const handle = await store.put(ctx, "conformance.v1.NoteFile.file", body);
    expect(fake.putChunks).toEqual([64 << 10, 64 << 10, 64 << 10, 8 << 10]);
    expect(fake.putEnds).toEqual([BigInt(200 << 10)]); // the end marker, last, with the body's size
    expect(handle.startsWith("note-files/" + createHash("sha256").update(body).digest("hex"))).toBe(true);
    const info = await store.stat(ctx, handle);
    expect(info.size).toBe(BigInt(body.length));
    expect(info.modTime.getTime()).toBe(1_700_000_000_123);
    expect(Buffer.compare(await store.read(ctx, handle), body)).toBe(0);
    const chunks: Uint8Array[] = [];
    for await (const c of store.open(ctx, handle)) chunks.push(c);
    expect(chunks.length).toBe(4);
    await store.delete(ctx, handle);
    const e1 = await store.read(ctx, handle).catch((x: unknown) => x);
    expect(files.isNotFound(e1)).toBe(true);
    const e2 = await store.stat(ctx, handle).catch((x: unknown) => x);
    expect(files.isNotFound(e2)).toBe(true);
    for (const [, seen] of fake.calls) expect(seen.incoming.first("w17-tx-id")).toBeUndefined();
  });

  it("put bodies are re-chunked", async () => {
    const store = clients.store("notes-files");
    const parts = () => [new Uint8Array(100_000).fill(97), Buffer.alloc(30_000, 98), new Uint8Array(10).fill(99)];
    async function* aparts() {
      for (const p of parts()) yield p;
    }
    const want = Buffer.concat(parts());
    for (const body of [parts(), aparts()]) {
      fake.putChunks = [];
      const handle = await store.put(new Context(), "NoteFile.file", body);
      expect(fake.putChunks).toEqual([64 << 10, 130_010 - (64 << 10)]);
      expect(Buffer.compare(await store.read(new Context(), handle), want)).toBe(0);
    }
    fake.putChunks = [];
    fake.putEnds = [];
    const handle = await store.put(new Context(), "NoteFile.file", new Uint8Array());
    expect(fake.putChunks).toEqual([]);
    expect(fake.putEnds).toEqual([0n]);
    expect((await store.read(new Context(), handle)).length).toBe(0);
  });

  it("put refusals", async () => {
    const store = clients.store("notes-files");
    const e = await store.put(new Context(), "NoteFile.file", new Uint8Array(262144 + 1)).catch((x: unknown) => x);
    expect((e as StatusError).code).toBe(Status.INVALID_ARGUMENT);
    expect(errorInfo(e)?.reason).toBe("FILE_TOO_LARGE");
    await expect(store.put(new Context(), "Nope.file", new Uint8Array(1))).rejects.toThrow(/not a field this tier writes files for/);
    const media = clients.store("notes-media");
    expect(media.writable).toBe(false);
    await expect(media.put(new Context(), "NoteFile.file", new Uint8Array(1))).rejects.toThrow(/stored on notes-files, not on notes-media/);
    const n = fake.calls.length;
    await expect(media.delete(new Context(), "any")).rejects.toThrow(/notes-media/);
    expect(fake.calls.length).toBe(n);
    await expect(store.put(new Context(), "NoteFile.file", "text" as never)).rejects.toThrow(TypeError);
    function* broken(): Generator<Uint8Array> {
      yield new Uint8Array(1);
      throw new Error("the author's file is gone");
    }
    const stored = fake.objects.size;
    fake.putEnds = [];
    await expect(store.put(new Context(), "NoteFile.file", broken())).rejects.toThrow(/the author's file is gone/);
    await new Promise((r) => setTimeout(r, 100));
    // A failed body never sends the end marker, so the companion stores nothing.
    expect(fake.putEnds).toEqual([]);
    expect(fake.objects.size).toBe(stored);
  });
});

it("a short field name must be unambiguous", () => {
  const ps = load(MANIFEST).companion!.files!.placements;
  const other = { ...ps[0]!, field: "other.v1.NoteFile.file" };
  expect(files.resolveField("NoteFile.file", ps).field).toBe("conformance.v1.NoteFile.file");
  expect(() => files.resolveField("NoteFile.file", [...ps, other])).toThrow(/ambiguous/);
  expect(() => files.resolveField("file", ps)).toThrow(RangeError);
});

it("boot refusals", () => {
  const p = pool();
  let d = conformance();
  d.companion.egress[0].service = "clients.nope.NopeApi";
  expect(() => new Clients(p, fromObject(d), { NOTES_COMPANION_ADDR: "127.0.0.1:1" })).toThrow(BootError);
  d = conformance();
  d.companion.default_address = "";
  expect(() => new Clients(p, fromObject(d), {})).toThrow(/NOTES_COMPANION_ADDR is not set/);
  expect(() => new Clients(p, load(MANIFEST), { NOTES_COMPANION_ADDR: "nowhere" })).toThrow(/has no port/);
  const c = new Clients(p, load(MANIFEST), {});
  expect(c.egress("echo").conn).toBeInstanceOf(CompanionConn);
  expect(c.egress("echo").conn.addr).toBe("notes-business-companion:50051");
  c.close();
});
