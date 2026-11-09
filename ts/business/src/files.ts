/**
 * File storage: the domain's upload connections, served by the business companion.
 *
 * A Go business tier stores and reads files through `sdk/go/lib/kvfs`
 * in-process; this tier calls the same Go code in the companion over
 * `w17.common.files.W17Files`, so a file is laid out, named and read back
 * exactly as in Go. `clients.store("app-exports")` is one connection:
 *
 *     const store = clients.store("app-exports");
 *     const handle = await store.put(ctx, "app.example.NoteExport.file", body); // kvfs.Put
 *     const data = await store.read(ctx, handle);                              // whole object
 *     for await (const chunk of store.open(ctx, handle)) { … }                 // streamed
 *     const info = await store.stat(ctx, handle);                              // { size, modTime }
 *     await store.delete(ctx, handle);                                         // writable only
 *
 * The handle is the value the field's column holds — byte for byte what Go's
 * `kvfs.Put` returns; store it as it is and pass column values back as they
 * are. A file is not part of a storage transaction (as in Go): a file put
 * inside one that then rolls back stays, so delete it yourself when the unit
 * of work fails.
 *
 * `isNotFound(err)` is Go's `errors.Is(err, kvfs.ErrNotFound)`.
 */

import { create, type DescMessage, type Message } from "@bufbuild/protobuf";
import type { CompanionConn } from "./client.js";
import type { Context } from "./context.js";
import { type AnyMessage, type MethodInfo, type Pool, getDefaultPool } from "./descriptors.js";
import { errorInfo } from "./errors.js";
import type { FileConnection, FilePlacement } from "./manifest.js";

export const SERVICE = "w17.common.files.W17Files";
/** The ErrorInfo domain of the companion's file errors. */
export const DOMAIN = "w17.files";
/** The size of the chunks `put` sends. */
export const CHUNK_SIZE = 64 << 10;

/** What `put` takes: bytes, or an (async) iterable of bytes — a Node `Readable` is one. */
export type Body = Uint8Array | Iterable<Uint8Array> | AsyncIterable<Uint8Array>;

/** A stored object's size and modification time (Go's `kvfs.Info`). */
export interface FileInfo {
  readonly size: bigint;
  readonly modTime: Date;
}

/**
 * True for the companion's "no stored file has this handle"
 * (`FILE_NOT_FOUND`) — Go's `errors.Is(err, kvfs.ErrNotFound)`.
 */
export function isNotFound(err: unknown): boolean {
  const info = errorInfo(err);
  return info !== undefined && info.domain === DOMAIN && info.reason === "FILE_NOT_FOUND";
}

/** The placement of `field`: its full proto name, or `Message.field` when that names exactly one placement. */
export function resolveField(field: string, placements: readonly FilePlacement[]): FilePlacement {
  const name = field.replace(/^\./, "");
  const exact = placements.find((p) => p.field === name);
  if (exact !== undefined) return exact;
  const matches = name.includes(".") ? placements.filter((p) => p.field.endsWith("." + name)) : [];
  if (matches.length === 1) return matches[0]!;
  if (matches.length === 0) {
    const known = placements.map((p) => p.field).sort().join(", ") || "none";
    throw new RangeError(`files: ${JSON.stringify(field)} is not a field this tier writes files for (have: ${known})`);
  }
  throw new RangeError(`files: ${JSON.stringify(field)} is ambiguous (${matches.map((p) => p.field).sort().join(", ")})`);
}

/** One upload connection's store, as the companion serves it. */
export class FileStore {
  readonly #conn: CompanionConn;
  readonly #connection: FileConnection;
  readonly #placements: readonly FilePlacement[];
  readonly #put: MethodInfo;
  readonly #open: MethodInfo;
  readonly #stat: MethodInfo;
  readonly #delete: MethodInfo;

  /** `placements` are every placement of the manifest. */
  constructor(conn: CompanionConn, connection: FileConnection, placements: readonly FilePlacement[], pool: Pool = getDefaultPool()) {
    this.#conn = conn;
    this.#connection = connection;
    this.#placements = placements;
    const ms = new Map(pool.methods(SERVICE).map((m) => [m.name, m]));
    this.#put = ms.get("Put")!;
    this.#open = ms.get("Open")!;
    this.#stat = ms.get("Stat")!;
    this.#delete = ms.get("Delete")!;
  }

  get name(): string {
    return this.#connection.name;
  }

  /** True when this tier writes the connection (`upload.writer: BUSINESS`); one the gateway writes is read-only here. */
  get writable(): boolean {
    return this.#connection.writable;
  }

  #requireWritable(what: string): void {
    if (!this.writable) {
      throw new Error(`files: ${what} on ${this.name}: the gateway writes this connection; this tier only reads it`);
    }
  }

  /**
   * Store `body` as a new object of `field` (full proto name, or
   * `Message.field` when unambiguous) and return its handle. The body is sent
   * in 64 KiB chunks. Nothing is stored unless the body is read to its end: a
   * body that throws (its error is rethrown as it is), a cancelled `ctx` or
   * one past its deadline leaves no object behind.
   */
  async put(ctx: Context, field: string, body: Body): Promise<string> {
    const p = resolveField(field, this.#placements);
    if (p.connection !== this.name) throw new RangeError(`files: ${p.field} is stored on ${p.connection}, not on ${this.name}`);
    this.#requireWritable(`put ${p.field}`);
    const input = this.#put.input;
    async function* requests(): AsyncGenerator<Message> {
      yield create(input, { part: { case: "header", value: { field: p.field } } });
      let size = 0n;
      for await (const chunk of chunks(body)) {
        size += BigInt(chunk.length);
        yield create(input, { part: { case: "chunk", value: chunk } });
      }
      // Only the end marker commits: a body that failed, or a call that was
      // cancelled or ran out of time, never sends it, and the companion then
      // stores nothing (grpc-js half-closes a cancelled call before it resets
      // it, so the close alone cannot mean "complete").
      yield create(input, { part: { case: "end", value: { size } } });
    }
    const reply = await this.#conn.clientStream(ctx, this.#put, requests());
    return String(reply.handle);
  }

  /** The stored object's bytes, as chunks of at most 64 KiB. */
  async *open(ctx: Context, handle: string): AsyncGenerator<Uint8Array> {
    for await (const msg of this.#conn.serverStream(ctx, this.#open, this.#req(this.#open.input, handle))) {
      const c = msg.chunk as Uint8Array;
      if (c.length) yield c;
    }
  }

  /** The whole stored object, in memory. */
  async read(ctx: Context, handle: string): Promise<Uint8Array> {
    const parts: Uint8Array[] = [];
    for await (const c of this.open(ctx, handle)) parts.push(c);
    return Buffer.concat(parts);
  }

  async stat(ctx: Context, handle: string): Promise<FileInfo> {
    const { reply } = await this.#conn.unary(ctx, this.#stat, this.#req(this.#stat.input, handle));
    const ts = reply.modTime as AnyMessage | undefined;
    const ms = ts === undefined ? 0 : Number(ts.seconds as bigint) * 1000 + Math.floor((ts.nanos as number) / 1e6);
    return { size: BigInt(reply.size as bigint), modTime: new Date(ms) };
  }

  /** Remove a stored object; a missing one is not an error. */
  async delete(ctx: Context, handle: string): Promise<void> {
    this.#requireWritable("delete");
    await this.#conn.unary(ctx, this.#delete, this.#req(this.#delete.input, handle));
  }

  #req(input: DescMessage, handle: string): Message {
    return create(input, { connection: this.name, handle });
  }
}

/** body re-chunked to CHUNK_SIZE. */
async function* chunks(body: Body): AsyncGenerator<Uint8Array> {
  if (body instanceof Uint8Array) {
    for (let i = 0; i < body.length; i += CHUNK_SIZE) yield body.subarray(i, i + CHUNK_SIZE);
    return;
  }
  if (typeof body === "string" || body === null || typeof body !== "object" ||
      !(Symbol.asyncIterator in body || Symbol.iterator in body)) {
    throw new TypeError(`files: a body is a Uint8Array or an (async) iterable of them, not ${typeof body}`);
  }
  let buf = new Uint8Array(CHUNK_SIZE);
  let n = 0;
  for await (const part of body as AsyncIterable<Uint8Array>) {
    if (!(part instanceof Uint8Array)) throw new TypeError(`files: a body yields Uint8Array, not ${typeof part}`);
    let off = 0;
    while (off < part.length) {
      const take = Math.min(CHUNK_SIZE - n, part.length - off);
      buf.set(part.subarray(off, off + take), n);
      n += take;
      off += take;
      if (n === CHUNK_SIZE) {
        yield buf;
        buf = new Uint8Array(CHUNK_SIZE);
        n = 0;
      }
    }
  }
  if (n > 0) yield buf.subarray(0, n);
}
