/**
 * Events: the envelope, its context data, and the brokers — byte-compatible
 * with sdk/go/lib/eventbus and the generated Go emit helpers.
 *
 * The envelope is the domain's events-lock message (`<pkg>.Event`): the event
 * in its `payload` oneof, `metadata` (50001) with the originating request's
 * metadata, `labels` (50002) with its broadcast labels. A Go subscriber
 * decodes what this module publishes exactly as it decodes a Go emit
 * (protobuf-es writes fields in field-number order, as Go does).
 *
 * Brokers, as the Go adapters address them:
 *
 * - NATS JetStream: stream `<channel>` over subjects `<channel>.>` (limits
 *   retention, discard old, max age 7 days, no count limit — created or
 *   updated on first use, as Go's `ensureStream`), subject
 *   `<channel>.<topic>`, payload the marshalled envelope, header
 *   `W17-Domain: <domain>` (the manifest's domain);
 * - Redis Streams: `XADD <channel> MINID ~ <now-7d>-0 * topic <topic> payload
 *   <envelope bytes> domain <domain>`.
 *
 * The domain stamp is Go's `NatsBusOptions.Domain` / `RedisBusOptions.Domain`:
 * two domains that declare the same channel name share its stream on one
 * broker, and a Go subscriber with a Domain settles unread a message stamped
 * with another domain. An empty domain stamps nothing (the wire before the
 * stamp existed).
 *
 * Emits are fire-and-forget: a failure is logged and reported, never thrown
 * into the RPC, and bounded by the publish timeout (2s).
 */

import { clone, create, toBinary, type DescField, type DescMessage, type Message } from "@bufbuild/protobuf";
import { scalarZeroValue } from "@bufbuild/protobuf/reflect";
import type { Context, Metadata } from "./context.js";
import { isCredentialKey, reservedMetadataKey } from "./credentials.js";
import type { AnyMessage, Pool } from "./descriptors.js";
import type { Emit, Invalidate } from "./manifest.js";
import { errorText, reportError } from "./observability.js";
import { LABEL_KEY_PREFIX, isGatewayOwnedKey } from "./principal.js";

export { isCredentialKey, reservedMetadataKey, LABEL_KEY_PREFIX };

/** eventbus.DefaultStreamMaxAge, in milliseconds. */
export const DEFAULT_STREAM_MAX_AGE_MS = 7 * 24 * 3600 * 1000;
/** eventbus.DefaultPublishTimeout, in milliseconds. */
export const DEFAULT_PUBLISH_TIMEOUT_MS = 2000;
export const MAX_CHANNEL_LEN = 200;
export const W17_INVALIDATE_TOPIC = "w17.invalidate";
/** eventbus.DomainHeader: the NATS header naming the emitting domain. */
export const DOMAIN_HEADER = "W17-Domain";

/** Tunables tests may lower. */
export const settings = { publishTimeoutMs: DEFAULT_PUBLISH_TIMEOUT_MS };

// --- names (eventbus/names.go) ---------------------------------------------

export class InvalidChannelError extends RangeError {
  override name = "InvalidChannelError";
}

export class InvalidTopicError extends RangeError {
  override name = "InvalidTopicError";
}

const nameChar = (c: string) => /^[A-Za-z0-9_-]$/.test(c);

export function validateChannel(name: string): void {
  if (!name) throw new InvalidChannelError("eventbus: invalid channel name: empty");
  if (Buffer.byteLength(name) > MAX_CHANNEL_LEN) {
    throw new InvalidChannelError(`eventbus: invalid channel name: ${JSON.stringify(name)} is longer than ${MAX_CHANNEL_LEN} characters`);
  }
  for (const c of name) {
    if (!nameChar(c)) {
      throw new InvalidChannelError(
        `eventbus: invalid channel name: ${JSON.stringify(name)} contains ${JSON.stringify(c)} (allowed: letters, digits, '_' and '-')`,
      );
    }
  }
  const low = name.toLowerCase();
  if (low === "w17" || low.startsWith("w17_")) {
    throw new InvalidChannelError(`eventbus: invalid channel name: ${JSON.stringify(name)} is reserved for the platform`);
  }
}

export function validateTopic(topic: string): void {
  if (!topic) throw new InvalidTopicError("eventbus: invalid topic: empty");
  for (const tok of topic.split(".")) {
    if (!tok) throw new InvalidTopicError(`eventbus: invalid topic: ${JSON.stringify(topic)} has an empty segment`);
    for (const c of tok) {
      if (c === "*" || c === ">" || /\s/u.test(c) || /\p{Cc}/u.test(c)) {
        throw new InvalidTopicError(`eventbus: invalid topic: ${JSON.stringify(topic)} contains ${JSON.stringify(c)}`);
      }
    }
  }
}

// --- context data (eventbus/eventctx.go) -------------------------------------

/** Incoming first, outgoing second, never a merge (eventbus.contextMD). */
function contextMd(ctx: Context): Metadata | undefined {
  if (ctx.incoming.size > 0) return ctx.incoming;
  if (ctx.outgoing.size > 0) return ctx.outgoing;
  return undefined;
}

/**
 * eventbus.MetadataFromContext: the request's metadata flattened for
 * `Event.metadata` — gateway-owned keys keep their FIRST value, every other
 * key its LAST non-empty one; transaction keys and credentials are dropped.
 */
export function metadataFromContext(ctx: Context): Record<string, string> | undefined {
  const md = contextMd(ctx);
  if (md === undefined) return undefined;
  const flat: Record<string, string> = {};
  for (const [k, vs] of md) {
    if (!vs.length || reservedMetadataKey(k)) continue;
    flat[k] = isGatewayOwnedKey(k) ? vs[0]! : lastNonEmpty(vs);
  }
  return flat;
}

/** The last value that is not blank, else the last value. */
function lastNonEmpty(vs: readonly string[]): string {
  for (let i = vs.length - 1; i >= 0; i--) {
    if (vs[i]!.trim() !== "") return vs[i]!;
  }
  return vs[vs.length - 1]!;
}

/**
 * eventbus.LabelsFromContext: `w17-label-*` with the prefix stripped, first
 * value; undefined when the event is unlabelled.
 */
export function labelsFromContext(ctx: Context): Record<string, string> | undefined {
  const md = contextMd(ctx);
  if (md === undefined) return undefined;
  let labels: Record<string, string> | undefined;
  for (const [k, vs] of md) {
    if (!vs.length || k.length <= LABEL_KEY_PREFIX.length || !k.startsWith(LABEL_KEY_PREFIX)) continue;
    labels ??= {};
    labels[k.slice(LABEL_KEY_PREFIX.length)] = vs[0]!;
  }
  return labels;
}

// --- the envelope ----------------------------------------------------------

function fieldByName(desc: DescMessage, name: string): DescField | undefined {
  return desc.fields.find((f) => f.name === name);
}

/** A field's raw value on a message, oneof members included (undefined when unset). */
function getField(msg: AnyMessage, f: DescField): unknown {
  if (f.oneof !== undefined) {
    const o = msg[f.oneof.localName] as { case?: string; value?: unknown } | undefined;
    return o?.case === f.localName ? o.value : undefined;
  }
  return msg[f.localName];
}

function setField(msg: AnyMessage, f: DescField, value: unknown): void {
  if (f.oneof !== undefined) msg[f.oneof.localName] = { case: f.localName, value };
  else msg[f.localName] = value;
}

function zeroOf(f: DescField): unknown {
  if (f.fieldKind === "scalar") return scalarZeroValue(f.scalar, false);
  if (f.fieldKind === "enum") return f.enum.values[0]?.number ?? 0;
  return undefined;
}

/**
 * Builds one emit's envelope from (request, response), as the generated Go
 * `emit<Svc><Method>` helper does.
 *
 * A payload binding reads a field path through the request or the response
 * (Go's chained getters: an unset message on the way reads as its zero value)
 * or takes a literal string. A proto3 `optional` target gets presence even
 * for a zero value, as Go's `proto.String(x)` wrap gives it; a message target
 * is copied only when the source message is set (Go assigns the pointer, nil
 * included).
 */
export class EnvelopeBuilder {
  readonly envelope: DescMessage;
  readonly event: DescMessage;
  readonly #payload: DescField;
  readonly #metadata: DescField;
  readonly #labels: DescField;
  readonly #pool: Pool;
  readonly emit: Emit;

  constructor(pool: Pool, emit: Emit) {
    this.#pool = pool;
    this.emit = emit;
    this.envelope = pool.schema(emit.envelope);
    this.event = pool.schema(emit.event);
    const payload = fieldByName(this.envelope, emit.envelopeField);
    if (payload === undefined) {
      throw new RangeError(`emit ${emit.method}: envelope ${emit.envelope} has no payload case ${JSON.stringify(emit.envelopeField)}`);
    }
    this.#payload = payload;
    const metadata = fieldByName(this.envelope, "metadata");
    const labels = fieldByName(this.envelope, "labels");
    if (metadata?.fieldKind !== "map") throw new RangeError(`emit ${emit.method}: envelope ${emit.envelope} has no metadata map`);
    if (labels?.fieldKind !== "map") throw new RangeError(`emit ${emit.method}: envelope ${emit.envelope} has no labels map`);
    this.#metadata = metadata;
    this.#labels = labels;
    for (const b of emit.fields) {
      if (fieldByName(this.event, b.field) === undefined) {
        throw new RangeError(`emit ${emit.method}: event ${emit.event} has no field ${JSON.stringify(b.field)}`);
      }
    }
  }

  build(ctx: Context, req: Message, resp: Message): AnyMessage {
    const ev = create(this.event) as AnyMessage;
    for (const b of this.emit.fields) {
      const target = fieldByName(this.event, b.field)!;
      if (b.source === "literal") {
        if (target.fieldKind !== "scalar" || typeof scalarZeroValue(target.scalar, false) !== "string") {
          throw new TypeError(`payload ${b.field}: a literal fills a string field only`);
        }
        setField(ev, target, b.value);
        continue;
      }
      const src = (b.source === "request" ? req : resp) as AnyMessage;
      const [value, present] = walk(this.#pool, src, b.path);
      assign(ev, target, value, present);
    }
    const env = create(this.envelope) as AnyMessage;
    const md = metadataFromContext(ctx);
    if (md) Object.assign(env[this.#metadata.localName] as object, md);
    const labels = labelsFromContext(ctx);
    if (labels) Object.assign(env[this.#labels.localName] as object, labels);
    setField(env, this.#payload, ev);
    return env;
  }

  /** The envelope, marshalled. */
  marshal(env: Message): Uint8Array {
    return toBinary(this.envelope, env);
  }
}

function walk(pool: Pool, msg: AnyMessage, path: readonly string[]): [unknown, boolean] {
  let cur: unknown = msg;
  let desc: DescMessage = pool.schema(msg.$typeName);
  let present = true;
  for (const seg of path) {
    const f = fieldByName(desc, seg);
    if (f === undefined) throw new RangeError(`payload path ${path.join(".")}: ${desc.typeName} has no field ${JSON.stringify(seg)}`);
    let v = getField(cur as AnyMessage, f);
    if (f.fieldKind === "message") {
      if (v === undefined) {
        present = false;
        v = create(f.message);
      }
      desc = f.message;
    } else if (v === undefined) {
      v = zeroOf(f);
    }
    cur = v;
  }
  return [cur, present];
}

function assign(ev: AnyMessage, f: DescField, value: unknown, present: boolean): void {
  if (f.fieldKind === "map") {
    Object.assign(ev[f.localName] as object, value as object);
    return;
  }
  if (f.fieldKind === "list") {
    (ev[f.localName] as unknown[]).push(...(value as unknown[]));
    return;
  }
  if (f.fieldKind === "message") {
    if (present && value !== undefined) setField(ev, f, clone(f.message, value as Message));
    return;
  }
  setField(ev, f, value);
}

// --- brokers ---------------------------------------------------------------

export interface Dispatcher {
  dispatch(ctx: Context, channel: string, topic: string, payload: Uint8Array): Promise<void>;
  close(): Promise<void>;
  /** Bounds one dispatch; the default is the publish timeout. */
  readonly publishTimeoutMs?: number;
}

/** Records dispatches in memory (tests, and a tier with no broker). */
export class MemoryPublisher implements Dispatcher {
  readonly published: [channel: string, topic: string, payload: Uint8Array][] = [];

  dispatch(_ctx: Context, channel: string, topic: string, payload: Uint8Array): Promise<void> {
    validateChannel(channel);
    validateTopic(topic);
    this.published.push([channel, topic, payload]);
    return Promise.resolve();
  }

  close(): Promise<void> {
    return Promise.resolve();
  }
}

function withTimeout<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  return Promise.race([
    p,
    new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${what}: timed out after ${ms}ms`)), ms);
    }),
  ]).finally(() => clearTimeout(timer));
}

/** The JetStream calls the publisher makes (a test may fake them). */
export interface JetStreamLike {
  updateStream(cfg: StreamSpec): Promise<void>;
  addStream(cfg: StreamSpec): Promise<void>;
  /** `headers` (when given) ride the message as NATS headers. */
  publish(subject: string, payload: Uint8Array, timeoutMs: number, headers?: Record<string, string>): Promise<void>;
  isClosed(): boolean;
  close(): Promise<void>;
}

/** The stream eventbus.ensureStream declares. */
export interface StreamSpec {
  name: string;
  subjects: string[];
  retention: "limits";
  discard: "old";
  /** Milliseconds, 0 for none. */
  maxAgeMs: number;
  maxMsgs: number;
}

/** A missing stream, or none answering on its subject. */
export class StreamMissingError extends Error {
  override name = "StreamMissingError";
}

/** Publishes to NATS JetStream as eventbus.NatsBus.Dispatch does. */
export class NatsPublisher implements Dispatcher {
  readonly publishTimeoutMs: number;
  readonly #streams = new Set<string>();
  readonly js: JetStreamLike;

  /** The emitting domain, stamped as the `W17-Domain` header; "" stamps nothing. */
  readonly domain: string;

  constructor(js: JetStreamLike, opts: { publishTimeoutMs?: number; maxAgeMs?: number; maxMsgs?: number; domain?: string } = {}) {
    this.js = js;
    this.domain = opts.domain ?? "";
    this.publishTimeoutMs = opts.publishTimeoutMs ?? DEFAULT_PUBLISH_TIMEOUT_MS;
    this.maxAgeMs = opts.maxAgeMs ?? DEFAULT_STREAM_MAX_AGE_MS;
    this.maxMsgs = opts.maxMsgs ?? -1;
  }

  readonly maxAgeMs: number;
  readonly maxMsgs: number;

  /**
   * Connect, retrying an unreachable broker for connectWaitMs
   * (eventbus.DefaultConnectWait: a broker started in the same deploy).
   */
  static async connect(dsn: string, opts: { connectWaitMs?: number; publishTimeoutMs?: number; domain?: string } = {}): Promise<NatsPublisher> {
    const o = natsConnectOptions(dsn);
    return new NatsPublisher(await natsJetStream(o, opts.connectWaitMs ?? 30_000), opts);
  }

  async #ensureStream(channel: string): Promise<void> {
    if (this.#streams.has(channel)) return;
    const cfg: StreamSpec = {
      name: channel,
      subjects: [`${channel}.>`],
      retention: "limits",
      discard: "old",
      maxAgeMs: this.maxAgeMs > 0 ? this.maxAgeMs : 0,
      maxMsgs: this.maxMsgs,
    };
    // CreateOrUpdateStream: update first, create when it does not exist.
    try {
      await this.js.updateStream(cfg);
    } catch (e) {
      if (!(e instanceof StreamMissingError)) throw e;
      await this.js.addStream(cfg);
    }
    this.#streams.add(channel);
  }

  async dispatch(_ctx: Context, channel: string, topic: string, payload: Uint8Array): Promise<void> {
    validateChannel(channel);
    validateTopic(topic);
    if (this.js.isClosed()) throw new Error("eventbus nats: connection is closed");
    const subject = `${channel}.${topic}`;
    const headers = this.domain ? { [DOMAIN_HEADER]: this.domain } : undefined;
    const go = async () => {
      await this.#ensureStream(channel);
      try {
        await this.js.publish(subject, payload, this.publishTimeoutMs, headers);
      } catch (e) {
        if (!(e instanceof StreamMissingError)) throw e;
        // The broker lost the stream (recreated without persistence):
        // forget it, re-create it, publish once more.
        this.#streams.delete(channel);
        await this.#ensureStream(channel);
        await this.js.publish(subject, payload, this.publishTimeoutMs, headers);
      }
    };
    await withTimeout(go(), this.publishTimeoutMs, `eventbus nats: publish ${subject}`);
  }

  async close(): Promise<void> {
    if (!this.js.isClosed()) await this.js.close();
  }
}

/** What a NATS DSN connects with. */
export interface NatsConnectOptions {
  /** `host:port`, one per server of the list. */
  servers: string[];
  user?: string;
  pass?: string;
  token?: string;
  tls: boolean;
}

/**
 * A NATS DSN as nats.go's `nats.Connect` (and nats.py) read it: a
 * comma-separated list of `nats://` / `tls://` URLs (the first must carry
 * its scheme; a later one without is `nats://`), port 4222 by default,
 * TLS when any of them is `tls://`. A URL's `user:pass@` is user and
 * password; a bare `user@` (no colon) is a TOKEN, not a user with an empty
 * password. nats.js takes one set of credentials for the connection: the
 * first URL that carries any supplies them, as the first server is the one
 * dialled first.
 */
export function natsConnectOptions(dsn: string): NatsConnectOptions {
  const out: NatsConnectOptions = { servers: [], tls: false };
  let creds = false;
  for (const raw of dsn.split(",")) {
    let s = raw.trim();
    if (!s) continue;
    // Past the first, a server without a scheme is nats://, as nats.go adds it.
    if (out.servers.length && !s.includes("://")) s = `nats://${s}`;
    const m = /^(nats|tls):\/\/(.*)$/.exec(s);
    if (m === null) throw new RangeError("NATS DSN must use nats:// or tls:// scheme");
    if (m[1] === "tls") out.tls = true;
    let url: URL;
    try {
      url = new URL(s);
    } catch {
      throw new RangeError(`NATS DSN: ${JSON.stringify(redactNats(s))} is not a URL`);
    }
    if (!url.hostname) throw new RangeError(`NATS DSN: ${JSON.stringify(redactNats(s))} names no host`);
    out.servers.push(`${url.hostname}:${url.port || "4222"}`);
    // The userinfo as written: URL cannot tell "user@" from "user:@".
    const authority = m[2]!.split(/[/?#]/, 1)[0]!;
    const at = authority.lastIndexOf("@");
    if (at < 0 || creds) continue;
    creds = true;
    const info = authority.slice(0, at);
    const colon = info.indexOf(":");
    if (colon < 0) out.token = decodeURIComponent(info);
    else {
      out.user = decodeURIComponent(info.slice(0, colon));
      out.pass = decodeURIComponent(info.slice(colon + 1));
    }
  }
  if (!out.servers.length) throw new RangeError("NATS DSN must use nats:// or tls:// scheme");
  return out;
}

/** A NATS URL without its credentials, for an error message. */
function redactNats(s: string): string {
  return s.replace(/^([a-z]+:\/\/)[^/?#]*@/i, "$1***@");
}

/** The real JetStream, behind {@link JetStreamLike}. */
async function natsJetStream(o: NatsConnectOptions, connectWaitMs: number): Promise<JetStreamLike> {
  const { connect, headers: natsHeaders } = await import("@nats-io/transport-node");
  const jsmod = await import("@nats-io/jetstream");
  const deadline = Date.now() + connectWaitMs;
  let nc: Awaited<ReturnType<typeof connect>>;
  for (;;) {
    try {
      nc = await connect({
        servers: o.servers,
        ...(o.user !== undefined && { user: o.user, pass: o.pass ?? "" }),
        ...(o.token !== undefined && { token: o.token }),
        ...(o.tls && { tls: {} }),
        reconnect: true,
        maxReconnectAttempts: -1,
        timeout: 2000,
      });
      break;
    } catch (e) {
      if (Date.now() >= deadline) throw new Error(`nats connect: ${String(e)}`, { cause: e });
      await new Promise((r) => setTimeout(r, 500));
    }
  }
  const jsm = await jsmod.jetstreamManager(nc);
  const js = jsmod.jetstream(nc);
  const isMissing = (e: unknown): boolean => {
    if (e instanceof jsmod.JetStreamApiError && e.code === jsmod.JetStreamApiCodes.StreamNotFound) return true;
    const name = (e as { name?: string } | null)?.name ?? "";
    const cause = (e as { cause?: { name?: string } } | null)?.cause?.name ?? "";
    return name === "StreamNotFoundError" || name === "NoRespondersError" || cause === "NoRespondersError" ||
      (e as { code?: unknown } | null)?.code === 503;
  };
  const toCfg = (s: StreamSpec) => ({
    name: s.name,
    subjects: s.subjects,
    retention: jsmod.RetentionPolicy.Limits,
    discard: jsmod.DiscardPolicy.Old,
    max_age: s.maxAgeMs * 1_000_000,
    max_msgs: s.maxMsgs,
  });
  return {
    async updateStream(s) {
      try {
        const { name, ...rest } = toCfg(s);
        await jsm.streams.update(name, rest);
      } catch (e) {
        if (isMissing(e)) throw new StreamMissingError(String(e), { cause: e });
        throw e;
      }
    },
    async addStream(s) {
      await jsm.streams.add(toCfg(s));
    },
    async publish(subject, payload, timeoutMs, headers) {
      try {
        let h: ReturnType<typeof natsHeaders> | undefined;
        if (headers !== undefined) {
          h = natsHeaders();
          for (const [k, v] of Object.entries(headers)) h.set(k, v);
        }
        await js.publish(subject, payload, { timeout: timeoutMs, ...(h !== undefined && { headers: h }) });
      } catch (e) {
        if (isMissing(e)) throw new StreamMissingError(String(e), { cause: e });
        throw e;
      }
    },
    isClosed: () => nc.isClosed(),
    async close() {
      await nc.drain();
    },
  };
}

/** The Redis calls the publisher makes (ioredis, or a test fake). */
export interface RedisLike {
  xadd(...args: (string | Buffer | number)[]): Promise<unknown>;
  xtrim(...args: (string | number)[]): Promise<unknown>;
  quit(): Promise<unknown>;
}

/** Appends to Redis Streams as eventbus.RedisBus.Dispatch does. */
export class RedisPublisher implements Dispatcher {
  readonly publishTimeoutMs: number;
  readonly maxAgeMs: number;
  readonly maxMsgs: number;
  readonly #clock: () => number;
  readonly client: RedisLike;
  /** The emitting domain, stamped as the entry's `domain` field; "" stamps nothing. */
  readonly domain: string;

  constructor(
    client: RedisLike,
    opts: { publishTimeoutMs?: number; maxAgeMs?: number; maxMsgs?: number; clock?: () => number; domain?: string } = {},
  ) {
    this.client = client;
    this.domain = opts.domain ?? "";
    this.publishTimeoutMs = opts.publishTimeoutMs ?? DEFAULT_PUBLISH_TIMEOUT_MS;
    this.maxAgeMs = opts.maxAgeMs ?? DEFAULT_STREAM_MAX_AGE_MS;
    this.maxMsgs = opts.maxMsgs ?? 0;
    this.#clock = opts.clock ?? Date.now;
  }

  /** `redis://` / `rediss://`; `cluster=true` selects a cluster client (lib/redisdsn). */
  static async connect(dsn: string, opts: { publishTimeoutMs?: number; domain?: string } = {}): Promise<RedisPublisher> {
    const u = new URL(dsn);
    if (u.protocol !== "redis:" && u.protocol !== "rediss:") {
      throw new RangeError(`redis DSN: scheme ${JSON.stringify(u.protocol.replace(/:$/, ""))}, want redis or rediss`);
    }
    let cluster = false;
    const vals = u.searchParams.getAll("cluster");
    if (vals.length) {
      if (vals.length !== 1 || !["1", "t", "true", "0", "f", "false"].includes(vals[0]!.toLowerCase())) {
        throw new RangeError("redis DSN: cluster=<bool> given more than once or not a bool");
      }
      cluster = ["1", "t", "true"].includes(vals[0]!.toLowerCase());
      if (cluster && !["", "/", "/0"].includes(u.pathname)) throw new RangeError("redis DSN: a cluster has database 0 only");
      u.searchParams.delete("cluster");
      if (cluster) u.pathname = "";
    }
    const { Redis, Cluster } = await import("ioredis");
    let client: RedisLike & { ping(): Promise<unknown> };
    if (cluster) {
      client = new (Cluster as unknown as new (...a: unknown[]) => RedisLike & { ping(): Promise<unknown> })([{ host: u.hostname, port: Number(u.port || 6379) }], {
        redisOptions: {
          ...(u.username && { username: decodeURIComponent(u.username) }),
          ...(u.password && { password: decodeURIComponent(u.password) }),
          ...(u.protocol === "rediss:" && { tls: {} }),
        },
      });
    } else {
      client = new Redis(u.toString(), { lazyConnect: false, maxRetriesPerRequest: 3 }) as unknown as RedisLike & { ping(): Promise<unknown> };
    }
    await withTimeout(client.ping(), 5000, "redis ping");
    return new RedisPublisher(client, opts);
  }

  async dispatch(_ctx: Context, channel: string, topic: string, payload: Uint8Array): Promise<void> {
    validateChannel(channel);
    validateTopic(topic);
    const args: (string | Buffer | number)[] = [channel];
    if (this.maxAgeMs > 0) args.push("MINID", "~", `${Math.trunc(this.#clock() - this.maxAgeMs)}-0`);
    else if (this.maxMsgs > 0) args.push("MAXLEN", "~", this.maxMsgs);
    args.push("*", "topic", topic, "payload", Buffer.from(payload));
    if (this.domain) args.push("domain", this.domain);
    const go = async () => {
      await this.client.xadd(...args);
      if (this.maxAgeMs > 0 && this.maxMsgs > 0) {
        try {
          await this.client.xtrim(channel, "MAXLEN", "~", this.maxMsgs);
        } catch {
          // best effort, the next append trims again
        }
      }
    };
    await withTimeout(go(), this.publishTimeoutMs, `eventbus redis: XADD ${channel}`);
  }

  async close(): Promise<void> {
    await this.client.quit();
  }
}

/**
 * Go's eventbus `Route`: a channel declared on Redis Streams goes to the
 * Redis bus, every other channel to the NATS bus. When the bus a channel
 * belongs to is not configured (its DSN is empty) the event is dropped —
 * never sent to the other bus, because the subscriber reads each channel
 * from its declared transport only.
 */
export class Router implements Dispatcher {
  readonly redisChannels: ReadonlySet<string>;
  readonly nats: Dispatcher | undefined;
  readonly redis: Dispatcher | undefined;

  constructor(nats: Dispatcher | undefined, redis: Dispatcher | undefined, redisChannels: readonly string[] = []) {
    this.nats = nats;
    this.redis = redis;
    this.redisChannels = new Set(redisChannels);
  }

  forChannel(channel: string): Dispatcher | undefined {
    return this.redisChannels.has(channel) ? this.redis : this.nats;
  }

  get publishTimeoutMs(): number {
    return Math.max(this.nats?.publishTimeoutMs ?? 0, this.redis?.publishTimeoutMs ?? 0) || DEFAULT_PUBLISH_TIMEOUT_MS;
  }

  async dispatch(ctx: Context, channel: string, topic: string, payload: Uint8Array): Promise<void> {
    const d = this.forChannel(channel);
    if (d === undefined) return;
    await d.dispatch(ctx, channel, topic, payload);
  }

  async close(): Promise<void> {
    for (const d of [this.nats, this.redis]) if (d !== undefined) await d.close();
  }
}

/**
 * eventbus.EmitInvalidate per entity: a bare `w17.InvalidateEvent` (not
 * wrapped in the envelope) with the request's labels and no ids.
 */
export function invalidateEvents(pool: Pool, inv: Invalidate, ctx: Context): AnyMessage[] {
  const schema = pool.schema("w17.InvalidateEvent");
  const labels = labelsFromContext(ctx) ?? {};
  return inv.entities.map((entity) => create(schema, { group: inv.group, message: entity, labels: { ...labels } }) as AnyMessage);
}

/**
 * Publish fire-and-forget: a failure is reported, never thrown (the RPC
 * already succeeded). Runs detached from the request's cancellation, bounded
 * by the dispatcher's publish timeout as Go bounds each Dispatch: a broker
 * that stopped answering must not hold the committing path (nor the drain).
 */
export async function fire(
  dispatcher: Dispatcher | undefined,
  ctx: Context,
  channel: string,
  topic: string,
  payload: Uint8Array,
  what = "",
): Promise<void> {
  if (dispatcher === undefined) return;
  const timeout = dispatcher.publishTimeoutMs ?? settings.publishTimeoutMs;
  try {
    await withTimeout(dispatcher.dispatch(ctx.withoutCancel(), channel, topic, payload), timeout, `${channel}/${topic}`);
  } catch (e) {
    reportError(ctx, new Error(`eventbus: emit ${channel}/${topic}${what ? ` for ${what}` : ""} failed: ${errorText(e)}`));
  }
}
