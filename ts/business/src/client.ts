/**
 * gRPC clients to the storage tier (and any other tier), built from descriptors.
 *
 * Every call does what Go's generated inter-tier client does (sdk/go/core/grpcx
 * `DialOpts`):
 *
 * - relays the verified principal (`principal.forwardToOutgoing`) — exact key
 *   set, replace semantics;
 * - sends the context's outgoing metadata (transaction routing, paging the
 *   author forwarded) and its deadline; a context already done is refused
 *   before anything is sent, as a gRPC client refuses it, and cancelling the
 *   context cancels the call;
 * - injects W3C trace context when OpenTelemetry is on;
 * - retries `UNAVAILABLE` and nothing else: 3 attempts, backoff 0.1s × 2 up
 *   to 1s, under the channel's retry throttle (100 tokens, 0.1 per success) —
 *   `grpcclient.DefaultServiceConfig`, which grpc-js applies as grpc-go does
 *   (a call is never retried once the server's response headers arrived);
 * - keeps the connection alive (ping every 30s, 10s timeout, also without calls);
 * - dials TLS per `W17_INTERNAL_TLS`.
 *
 * Every failure is thrown as a `StatusError` (a transport failure that
 * survives the retries as `UNAVAILABLE`, a deadline as `DEADLINE_EXCEEDED`),
 * so a caller handles one error type, as with grpc-go.
 */

import {
  Client,
  Metadata as GrpcMetadata,
  type CallOptions as GrpcCallOptions,
  type ChannelOptions,
  type ServiceError,
} from "@grpc/grpc-js";
import { fromBinary, toBinary, type DescMessage, type Message } from "@bufbuild/protobuf";
import { Context, Metadata } from "./context.js";
import { type AnyMessage, type MethodInfo, type Pool, getDefaultPool } from "./descriptors.js";
import { STATUS_DETAILS_KEY, Status, StatusError } from "./errors.js";
import * as observability from "./observability.js";
import * as paging from "./paging.js";
import * as principal from "./principal.js";
import * as tlsconfig from "./tlsconfig.js";
import type { Env } from "./secrets.js";

/** grpcclient.DefaultServiceConfig, verbatim. */
export const DEFAULT_SERVICE_CONFIG = {
  methodConfig: [
    {
      name: [{}],
      retryPolicy: {
        maxAttempts: 3,
        initialBackoff: "0.1s",
        maxBackoff: "1s",
        backoffMultiplier: 2.0,
        retryableStatusCodes: ["UNAVAILABLE"],
      },
    },
  ],
  retryThrottling: { maxTokens: 100, tokenRatio: 0.1 },
};

/** grpcclient.DefaultClientKeepalive: Time 30s, Timeout 10s, PermitWithoutStream. */
export const CLIENT_OPTIONS: ChannelOptions = {
  "grpc.service_config": JSON.stringify(DEFAULT_SERVICE_CONFIG),
  "grpc.enable_retries": 1,
  "grpc.keepalive_time_ms": 30_000,
  "grpc.keepalive_timeout_ms": 10_000,
  "grpc.keepalive_permit_without_calls": 1,
};

/** `host:port` / `[v6]:port` / `:port` (localhost). */
export function parseAddr(addr: string): [string, number] {
  let host: string;
  let port: string;
  if (addr.startsWith("[")) {
    const end = addr.indexOf("]");
    host = addr.slice(1, end < 0 ? undefined : end);
    port = end < 0 ? "" : addr.slice(end + 1).replace(/^:/, "");
  } else {
    const i = addr.lastIndexOf(":");
    host = i < 0 ? "" : addr.slice(0, i);
    port = i < 0 ? addr : addr.slice(i + 1);
  }
  if (!/^[0-9]+$/.test(port)) throw new RangeError(`address ${JSON.stringify(addr)} has no port`);
  return [host || "localhost", Number(port)];
}

function target(addr: string): string {
  const [host, port] = parseAddr(addr);
  return host.includes(":") ? `[${host}]:${port}` : `${host}:${port}`;
}

/** grpc-js metadata from our pairs (`-bin` keys carried as latin-1, as received). */
export function toGrpcMetadata(pairs: Iterable<readonly [string, string]>): GrpcMetadata {
  const md = new GrpcMetadata();
  for (const [k, v] of pairs) {
    if (k.endsWith("-bin")) md.add(k, Buffer.from(v, "latin1"));
    else md.add(k, v);
  }
  return md;
}

/** Our metadata from grpc-js's. */
export function fromGrpcMetadata(md: GrpcMetadata | undefined): Metadata {
  if (md === undefined) return new Metadata();
  const pairs: [string, string | Uint8Array][] = [];
  for (const [k, vs] of Object.entries(md.toJSON())) for (const v of vs) pairs.push([k, v]);
  return new Metadata(pairs);
}

/** A grpc-js error (or anything thrown by a call) as a StatusError. */
export function asStatusError(e: unknown, ctx?: Context): unknown {
  if (e instanceof StatusError) return e;
  const se = e as Partial<ServiceError> | null;
  if (se && typeof se.code === "number") {
    let code = se.code;
    // A call cancelled because its deadline passed reads as the deadline.
    if (code === Status.CANCELLED && ctx?.err() === "context deadline exceeded") code = Status.DEADLINE_EXCEEDED;
    const raw = se.metadata?.get(STATUS_DETAILS_KEY)[0];
    const details = raw === undefined ? undefined : typeof raw === "string" ? Buffer.from(raw, "latin1") : new Uint8Array(raw);
    return new StatusError(code, se.details ?? se.message ?? "", details);
  }
  return e;
}

export interface CallOptions {
  /** Bounds this call (milliseconds), on top of the context's deadline. */
  timeoutMs?: number;
}

/** One channel to one address, shared by every client on it. */
export class Conn {
  readonly addr: string;
  readonly client: Client;

  constructor(addr: string, opts: { tls?: boolean; environ?: Env; options?: ChannelOptions } = {}) {
    this.addr = addr;
    let c: tlsconfig.ClientTLS = opts.tls === false ? { tls: false } : tlsconfig.clientTLS(opts.environ ?? process.env);
    if (opts.tls === true && !c.tls) {
      // Forced on: the rest of the configuration (CA, client pair) is still the environment's.
      c = tlsconfig.clientTLS({ ...(opts.environ ?? process.env), [tlsconfig.ENV_INTERNAL_TLS]: "on" });
    }
    this.client = new Client(target(addr), tlsconfig.channelCredentials(c), { ...CLIENT_OPTIONS, ...opts.options });
  }

  close(): void {
    this.client.close();
  }

  #prepare(ctx: Context, timeoutMs?: number): { ctx: Context; md: GrpcMetadata; options: GrpcCallOptions } {
    if (timeoutMs !== undefined) ctx = ctx.withTimeout(timeoutMs);
    const err = ctx.err();
    if (err !== undefined) {
      throw new StatusError(err === "context deadline exceeded" ? Status.DEADLINE_EXCEEDED : Status.CANCELLED, err);
    }
    const out = principal.forwardToOutgoing(ctx);
    const md = toGrpcMetadata(observability.injectTrace(out, out.outgoing.pairs()));
    const rem = ctx.timeRemaining();
    const options: GrpcCallOptions = rem === undefined ? {} : { deadline: new Date(Date.now() + Math.max(rem, 0)) };
    return { ctx, md, options };
  }

  /** One unary call; resolves to the reply and the response headers. */
  unary(ctx0: Context, m: MethodInfo, req: Message, opts: CallOptions = {}): Promise<{ reply: AnyMessage; header: Metadata }> {
    const { ctx, md, options } = this.#prepare(ctx0, opts.timeoutMs);
    return new Promise((resolve, reject) => {
      let header: Metadata | undefined;
      const call = this.client.makeUnaryRequest(
        m.path,
        serializer(m.input),
        deserializer(m.output),
        req,
        md,
        options,
        (err, reply) => {
          off();
          if (err) {
            reject(asStatusError(err, ctx));
            return;
          }
          if (reply === undefined) {
            reject(new StatusError(Status.UNKNOWN, `${m.path}: no response message`));
            return;
          }
          // A call made with paging.forward relays the page position storage
          // reported onto the business method's response (Go's
          // paging.RelayPositionUnaryInterceptor).
          paging.relayPosition(ctx, header);
          resolve({ reply, header: header ?? new Metadata() });
        },
      );
      call.on("metadata", (h: GrpcMetadata) => {
        header = fromGrpcMetadata(h);
      });
      const off = cancelOnDone(ctx, () => call.cancel());
    });
  }

  async *serverStream(ctx0: Context, m: MethodInfo, req: Message): AsyncGenerator<AnyMessage> {
    const { ctx, md, options } = this.#prepare(ctx0);
    const call = this.client.makeServerStreamRequest(m.path, serializer(m.input), deserializer(m.output), req, md, options);
    const off = cancelOnDone(ctx, () => call.cancel());
    try {
      for await (const reply of call) yield reply as AnyMessage;
    } catch (e) {
      throw asStatusError(e, ctx);
    } finally {
      off();
      call.cancel();
    }
  }

  clientStream(ctx0: Context, m: MethodInfo, reqs: Iterable<Message> | AsyncIterable<Message>): Promise<AnyMessage> {
    const { ctx, md, options } = this.#prepare(ctx0);
    return new Promise((resolve, reject) => {
      const call = this.client.makeClientStreamRequest(
        m.path,
        serializer(m.input),
        deserializer(m.output),
        md,
        options,
        (err, reply) => {
          off();
          if (err) reject(asStatusError(err, ctx));
          else if (reply === undefined) reject(new StatusError(Status.UNKNOWN, `${m.path}: no response message`));
          else resolve(reply);
        },
      );
      const off = cancelOnDone(ctx, () => call.cancel());
      sendAll(call, reqs).catch((e: unknown) => {
        call.cancel();
        reject(e);
      });
    });
  }

  async *bidiStream(ctx0: Context, m: MethodInfo, reqs: Iterable<Message> | AsyncIterable<Message>): AsyncGenerator<AnyMessage> {
    const { ctx, md, options } = this.#prepare(ctx0);
    const call = this.client.makeBidiStreamRequest(m.path, serializer(m.input), deserializer(m.output), md, options);
    const off = cancelOnDone(ctx, () => call.cancel());
    let sendErr: unknown;
    const sender = sendAll(call, reqs).catch((e: unknown) => {
      sendErr = e;
      call.cancel();
    });
    try {
      for await (const reply of call) yield reply as AnyMessage;
      await sender;
      if (sendErr !== undefined) throw sendErr;
    } catch (e) {
      throw sendErr ?? asStatusError(e, ctx);
    } finally {
      off();
      call.cancel();
    }
  }
}

/** Cancel the call when ctx is cancelled; a passed deadline is grpc-js's to end. */
function cancelOnDone(ctx: Context, cancel: () => void): () => void {
  return ctx.onDone(() => {
    if (ctx.err() === "context canceled") cancel();
  });
}

interface Writable {
  write(m: Message): boolean;
  end(): void;
  once(ev: "drain", fn: () => void): unknown;
}

async function sendAll(call: Writable, reqs: Iterable<Message> | AsyncIterable<Message>): Promise<void> {
  for await (const r of reqs) {
    if (!call.write(r)) await new Promise<void>((res) => call.once("drain", res));
  }
  call.end();
}

const serializers = new WeakMap<DescMessage, (m: Message) => Buffer>();
const deserializers = new WeakMap<DescMessage, (b: Buffer) => AnyMessage>();

export function serializer(d: DescMessage): (m: Message) => Buffer {
  let f = serializers.get(d);
  if (f === undefined) {
    f = (m: Message) => Buffer.from(toBinary(d, m));
    serializers.set(d, f);
  }
  return f;
}

export function deserializer(d: DescMessage): (b: Buffer) => AnyMessage {
  let f = deserializers.get(d);
  if (f === undefined) {
    f = (b: Buffer) => fromBinary(d, b) as AnyMessage;
    deserializers.set(d, f);
  }
  return f;
}

 
/**
 * A client of one gRPC service; its methods are the RPCs, by exact name:
 *
 *     const note = await clients.service("notes.v1.NoteStorage").CreateNote(ctx, req);
 *
 * Unary → a promise of the reply; server-streaming → an async iterator;
 * client-streaming → a promise, taking an iterable of requests; bidi → an
 * async iterator taking an iterable.
 */
export class ServiceClient {
  [rpc: string]: any;
  readonly #conn: Conn;
  readonly #service: string;
  readonly #methods: ReadonlyMap<string, MethodInfo>;

  constructor(conn: Conn, service: string, pool: Pool = getDefaultPool()) {
    this.#conn = conn;
    this.#service = service;
    const methods = pool.methods(service);
    this.#methods = new Map(methods.map((m) => [m.name, m]));
    for (const m of methods) {
      let call: (...a: any[]) => any;
      if (!m.clientStreaming && !m.serverStreaming) {
        call = async (ctx: Context, req: Message, opts?: CallOptions) => (await conn.unary(ctx, m, req, opts)).reply;
      } else if (m.serverStreaming && !m.clientStreaming) {
        call = (ctx: Context, req: Message) => conn.serverStream(ctx, m, req);
      } else if (m.clientStreaming && !m.serverStreaming) {
        call = (ctx: Context, reqs: Iterable<Message> | AsyncIterable<Message>) => conn.clientStream(ctx, m, reqs);
      } else {
        call = (ctx: Context, reqs: Iterable<Message> | AsyncIterable<Message>) => conn.bidiStream(ctx, m, reqs);
      }
      Object.defineProperty(call, "name", { value: m.name });
      Object.defineProperty(this, m.name, { value: call, enumerable: true });
    }
  }

  get service(): string {
    return this.#service;
  }

  get conn(): Conn {
    return this.#conn;
  }

  methodInfo(name: string): MethodInfo {
    const m = this.#methods.get(name);
    if (m === undefined) throw new RangeError(`${this.#service} has no rpc ${JSON.stringify(name)}`);
    return m;
  }
}
 

/** The `W17DistributedTransaction` client distx drives (`distx.TxClient`). */
export class TxCoordinator {
  static readonly SERVICE = "w17.common.distx.W17DistributedTransaction";
  readonly #conn: Conn;
  readonly #begin: MethodInfo;
  readonly #commit: MethodInfo;
  readonly #rollback: MethodInfo;

  constructor(conn: Conn) {
    this.#conn = conn;
    const ms = new Map(getDefaultPool().methods(TxCoordinator.SERVICE).map((m) => [m.name, m]));
    this.#begin = ms.get("Begin")!;
    this.#commit = ms.get("Commit")!;
    this.#rollback = ms.get("Rollback")!;
  }

  begin(ctx: Context, req: Message): Promise<{ reply: AnyMessage; header: Metadata }> {
    return this.#conn.unary(ctx, this.#begin, req);
  }

  async commit(ctx: Context, req: Message): Promise<AnyMessage> {
    return (await this.#conn.unary(ctx, this.#commit, req)).reply;
  }

  async rollback(ctx: Context, req: Message): Promise<AnyMessage> {
    return (await this.#conn.unary(ctx, this.#rollback, req)).reply;
  }
}

export function dial(addr: string, opts: { tls?: boolean } = {}): Conn {
  return new Conn(addr, opts);
}
