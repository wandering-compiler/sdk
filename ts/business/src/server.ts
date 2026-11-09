/**
 * Serving business methods: the registry, the middleware, the grpc-js mapping.
 *
 * A handler is an object whose methods are named after the RPCs — the EXACT
 * proto name, as in Go (`CreateNote`, not `createNote`), so one name means
 * one thing in the proto, the gateway, the logs and the code:
 *
 *     class NoteService {
 *       async CreateNote(ctx, req) { ... }              // unary
 *       async *WatchNotes(ctx, req) { yield ... }       // server-streaming: an async generator
 *       async Upload(ctx, requests) {                   // client-streaming:
 *         for await (const r of requests) { ... }       //   an async iterable in
 *       }
 *       async *Chat(ctx, requests) {                    // bidi: both
 *         for await (const r of requests) yield ...
 *       }
 *     }
 *
 * `registry.register("notes.v1.NoteService", new NoteService(...))` checks
 * every RPC of the service against the object and refuses
 * (`RegistrationError`, which stops the boot) when one is missing or is not
 * an `async` function (an `async *` generator for a server-streaming one).
 *
 * Every unary method runs through the same chain, over the wire AND through
 * `registry.self(...)`:
 *
 * 1. the author's method;
 * 2. the emit middleware, when the manifest lists an emit for it: after the
 *    method returns, the event is published — or, inside a transaction,
 *    parked until the root commits and dropped if it rolls back (`txscope`).
 *
 * **Compose your own methods through `registry.self(Service)`**, which
 * returns the served service with its middleware. A direct call on your
 * object skips the emit, and nothing fails — exactly the Go rule.
 *
 * Over the wire the runtime adds, outside that chain: the context (incoming
 * metadata, deadline, method), an OpenTelemetry server span, and the mapping
 * of errors to statuses (see `errors`). No emit on a streaming method, as in
 * Go.
 */

import { performance } from "node:perf_hooks";
import { toBinary, type Message } from "@bufbuild/protobuf";
import {
  Metadata as GrpcMetadata,
  type MethodDefinition,
  type Server,
  type ServerDuplexStream,
  type ServerReadableStream,
  type ServerUnaryCall,
  type ServerWritableStream,
  type ServiceError,
  type UntypedHandleCall,
  type UntypedServiceImplementation,
  type sendUnaryData,
} from "@grpc/grpc-js";
import { deserializer, fromGrpcMetadata, serializer, toGrpcMetadata } from "./client.js";
import { ConfigError } from "./config.js";
import { Context, cancelCall } from "./context.js";
import type { AnyMessage, MethodInfo, Pool } from "./descriptors.js";
import { DistxError } from "./distx.js";
import { STATUS_DETAILS_KEY, Status, StatusError, UserError, encodeRpcStatus, pack } from "./errors.js";
import * as events from "./events.js";
import type { Emit, Invalidate } from "./manifest.js";
import * as observability from "./observability.js";
import * as paging from "./paging.js";
import { NoPrincipalError } from "./principal.js";
import * as txscope from "./txscope.js";

/** The registry cannot serve what it was asked to; stops the boot. */
export class RegistrationError extends Error {
  override name = "RegistrationError";
}

/** Errors the SDK throws that Go would RETURN (not panic with): answered UNKNOWN with their message. */
function isReturned(e: unknown): e is Error {
  return e instanceof DistxError || e instanceof NoPrincipalError || e instanceof ConfigError;
}

 
type Fn = (...args: any[]) => any;

const tag = (fn: unknown) => Object.prototype.toString.call(fn);
const isAsyncFunction = (fn: unknown) => tag(fn) === "[object AsyncFunction]";
const isAsyncGeneratorFunction = (fn: unknown) => tag(fn) === "[object AsyncGeneratorFunction]";

/** One served RPC: the author's bound method plus the middleware. */
class MethodImpl {
  readonly builder: events.EnvelopeBuilder | undefined;
  readonly info: MethodInfo;
  readonly fn: Fn;
  readonly emit: Emit | undefined;
  readonly invalidates: readonly Invalidate[];
  readonly registry: Registry;

  constructor(info: MethodInfo, fn: Fn, emit: Emit | undefined, invalidates: readonly Invalidate[], registry: Registry) {
    this.info = info;
    this.fn = fn;
    this.emit = emit;
    this.invalidates = invalidates;
    this.registry = registry;
    this.builder = emit !== undefined ? new events.EnvelopeBuilder(registry.pool, emit) : undefined;
  }

  readonly invokeUnary = async (ctx: Context, req: Message): Promise<AnyMessage> => {
    const resp = (await this.fn(ctx, req)) as AnyMessage | undefined | null;
    if (resp === undefined || resp === null) {
      throw new StatusError(Status.INTERNAL, `${this.info.path}: handler returned no response`);
    }
    if (this.builder !== undefined || this.invalidates.length) await this.#emit(ctx, req, resp);
    return resp;
  };

  /**
   * The Go emit wrapper: after success, publish — or, inside a transaction,
   * park until the root commits (dropped on rollback). The emit goes first,
   * the invalidates after it, as the Go helper orders them.
   */
  async #emit(ctx: Context, req: Message, resp: Message): Promise<void> {
    const reg = this.registry;
    const { emit, builder, invalidates } = this;
    const path = this.info.path;
    const deferred = ctx.withoutCancel();
    const publish = async (c: Context) => {
      if (emit !== undefined && builder !== undefined) {
        let payload: Uint8Array | undefined;
        try {
          payload = builder.marshal(builder.build(c, req, resp));
        } catch (e) {
          // an emit never fails the RPC
          observability.reportError(c, new Error(`eventbus: build the ${path} event: ${observability.errorText(e)}`));
        }
        if (payload !== undefined) await events.fire(reg.dispatcher, c, emit.channel, emit.topic, payload, path);
      }
      const schema = reg.pool.schema("w17.InvalidateEvent");
      for (const inv of invalidates) {
        for (const ev of events.invalidateEvents(reg.pool, inv, c)) {
          await events.fire(reg.dispatcher, c, inv.channel, inv.topic, toBinary(schema, ev), path);
        }
      }
    };
    const out = txscope.afterCommit(ctx, () => publish(deferred));
    if (out === txscope.Outcome.NO_SCOPE || out === txscope.Outcome.ALREADY_COMMITTED) {
      // The write behind it has landed: a caller that hangs up now (a client
      // hang-up, a deadline) must not take the event with it.
      await txscope.shielded(publish(deferred));
    } else if (out === txscope.Outcome.DISCARDED) {
      observability.reportError(ctx, "txregistry: dropped an announcement made after its in-process transaction rolled back");
    }
  }
}

/**
 * A served service: its RPCs, with the middleware, by exact name.
 *
 * `registry.register` returns it and `registry.self` finds it again:
 *
 *     const note = await this.me.CreateNote(ctx, req);   // emits, as over the wire
 */
export class Served {
  [rpc: string]: any;
  readonly #service: string;
  readonly #methods: ReadonlyMap<string, MethodImpl>;

  /** @internal */
  constructor(service: string, methods: ReadonlyMap<string, MethodImpl>) {
    this.#service = service;
    this.#methods = methods;
    for (const m of methods.values()) {
      const call = !m.info.clientStreaming && !m.info.serverStreaming ? m.invokeUnary : m.fn;
      Object.defineProperty(this, m.info.name, { value: call, enumerable: true });
    }
  }

  get service(): string {
    return this.#service;
  }

  /** The gRPC paths it serves. */
  paths(): string[] {
    return [...this.#methods.values()].map((m) => m.info.path);
  }

  /** @internal */
  methodImpls(): MethodImpl[] {
    return [...this.#methods.values()];
  }
}
 

export type ShutdownHook = (ctx: Context) => unknown;

/**
 * What `registerBusiness(cfg, registry, clients)` registers into.
 *
 * `services`, when given (the manifest's list), is what the registry may
 * serve: registering any other service is refused. `emits` and `invalidates`
 * are keyed by the method's gRPC path.
 */
export class Registry {
  readonly pool: Pool;
  dispatcher: events.Dispatcher | undefined;
  readonly #emits: ReadonlyMap<string, Emit>;
  readonly #invalidates = new Map<string, Invalidate[]>();
  readonly #allowed: ReadonlySet<string> | undefined;
  readonly #served = new Map<string, Served>();
  readonly #shutdowns: ShutdownHook[] = [];
  draining = false;
  #inflight = 0;
  #idle: (() => void)[] = [];

  constructor(
    pool: Pool,
    opts: {
      emits?: readonly Emit[];
      dispatcher?: events.Dispatcher | undefined;
      invalidates?: readonly Invalidate[];
      services?: readonly string[];
    } = {},
  ) {
    this.pool = pool;
    this.dispatcher = opts.dispatcher;
    this.#emits = new Map((opts.emits ?? []).map((e) => [e.method, e]));
    for (const inv of opts.invalidates ?? []) {
      const l = this.#invalidates.get(inv.method) ?? [];
      l.push(inv);
      this.#invalidates.set(inv.method, l);
    }
    this.#allowed = opts.services === undefined ? undefined : new Set(opts.services.map((s) => s.replace(/^\./, "")));
  }

  // --- registration ------------------------------------------------------

  #resolveService(service: string): string {
    const name = service.replace(/^\./, "");
    try {
      return this.pool.service(name).typeName;
    } catch {
      // a short name, maybe
    }
    const matches = this.pool.servicesNamed(name);
    if (matches.length === 1) return matches[0]!;
    if (!matches.length) throw new RegistrationError(`registry: no service ${JSON.stringify(name)} in the descriptor set`);
    throw new RegistrationError(`registry: ${JSON.stringify(name)} is ambiguous (${matches.join(", ")}); use the full name`);
  }

  /**
   * Serve `impl` as `service` (full name, or short when unambiguous).
   *
   * Returns the served service — keep it to compose your own methods through
   * the middleware (or find it again with `self`).
   */
  register(service: string, impl: object): Served {
    const full = this.#resolveService(service);
    if (this.#allowed !== undefined && !this.#allowed.has(full)) {
      throw new RegistrationError(
        `registry: ${full} is not a business service of this tier (the manifest lists: ${[...this.#allowed].sort().join(", ") || "none"})`,
      );
    }
    if (this.#served.has(full)) throw new RegistrationError(`registry: ${full} is already registered`);
    const methods = new Map<string, MethodImpl>();
    const missing: string[] = [];
    const notAsync: string[] = [];
    for (const info of this.pool.methods(full)) {
      const fn = (impl as Record<string, unknown>)[info.name];
      if (typeof fn !== "function") {
        missing.push(info.name);
        continue;
      }
      const ok = info.serverStreaming ? isAsyncGeneratorFunction(fn) : isAsyncFunction(fn);
      if (!ok) {
        notAsync.push(`${info.name} (want ${info.serverStreaming ? "an async generator (async *)" : "an async function"})`);
        continue;
      }
      const emit = this.#emits.get(info.path);
      const invs = this.#invalidates.get(info.path) ?? [];
      if ((emit !== undefined || invs.length) && (info.clientStreaming || info.serverStreaming)) {
        throw new RegistrationError(`registry: ${info.path} is streaming and cannot emit an event`);
      }
      methods.set(info.name, new MethodImpl(info, (fn as Fn).bind(impl), emit, invs, this));
    }
    const cls = impl.constructor?.name ?? "the handler";
    if (missing.length) {
      throw new RegistrationError(
        `registry: ${cls} does not implement ${full}: missing method(s) ${missing.join(", ")} ` +
          "(handler methods are named exactly as the RPCs)",
      );
    }
    if (notAsync.length) throw new RegistrationError(`registry: ${cls} for ${full}: ${notAsync.join(", ")}`);
    const served = new Served(full, methods);
    this.#served.set(full, served);
    return served;
  }

  /**
   * The served `service` — call your own methods through this, never directly
   * on your object, or their events are skipped.
   *
   * It is Go's direct `s.self.Method(ctx, req)`: the callee gets the ctx you
   * pass UNCHANGED — your incoming metadata, your transaction (it joins), and
   * whatever you put on it. So pass a `paging.forward` ctx only to the
   * storage call that returns the page, never here (the callee's own calls
   * would carry the page and relay their position onto your response); and
   * `principal.actAs` on a ctx passed here is not what the callee's principal
   * readers see — it sets the OUTGOING side, they read the incoming one —
   * exactly as in Go.
   */
  self(service: string): Served {
    const full = this.#resolveService(service);
    const s = this.#served.get(full);
    if (s === undefined) throw new RegistrationError(`registry: ${full} is not registered (register it before composing it)`);
    return s;
  }

  /**
   * A drain hook: called with a context bounded by the drain timeout (30s)
   * after the server stopped serving. May be sync or async.
   */
  registerShutdown(fn: ShutdownHook): void {
    this.#shutdowns.push(fn);
  }

  get served(): Served[] {
    return [...this.#served.values()];
  }

  get shutdowns(): ShutdownHook[] {
    return [...this.#shutdowns];
  }

  // --- in-flight tracking (graceful drain) --------------------------------

  /** @internal */
  enter(): void {
    this.#inflight++;
  }

  /** @internal */
  leave(): void {
    this.#inflight--;
    if (this.#inflight === 0) {
      const waiters = this.#idle;
      this.#idle = [];
      for (const w of waiters) w();
    }
  }

  get inflight(): number {
    return this.#inflight;
  }

  /** Resolves true once no call is in flight, false after timeoutMs. */
  waitIdle(timeoutMs: number): Promise<boolean> {
    if (this.#inflight === 0) return Promise.resolve(true);
    return new Promise((resolve) => {
      const done = () => {
        clearTimeout(timer);
        resolve(true);
      };
      const timer = setTimeout(() => {
        this.#idle = this.#idle.filter((w) => w !== done);
        resolve(false);
      }, timeoutMs);
      this.#idle.push(done);
    });
  }

  /** Add every served service to a grpc-js server. */
  addTo(server: Server): void {
    for (const s of this.#served.values()) {
      const def: Record<string, MethodDefinition<unknown, unknown>> = {};
      const impl: UntypedServiceImplementation = {};
      for (const m of s.methodImpls()) {
        def[m.info.name] = methodDefinition(m.info);
        impl[m.info.name] = handler(m, this);
      }
      server.addService(def, impl);
    }
  }
}

/** A grpc-js method definition built from a method descriptor. */
export function methodDefinition(info: MethodInfo): MethodDefinition<unknown, unknown> {
  return {
    path: info.path,
    requestStream: info.clientStreaming,
    responseStream: info.serverStreaming,
    requestSerialize: serializer(info.input) as (v: unknown) => Buffer,
    requestDeserialize: deserializer(info.input),
    responseSerialize: serializer(info.output) as (v: unknown) => Buffer,
    responseDeserialize: deserializer(info.output),
    originalName: info.name,
  };
}

interface AnyCall {
  metadata: GrpcMetadata;
  getDeadline(): Date | number;
  on(ev: "cancelled", fn: () => void): unknown;
}

function contextFor(call: AnyCall, path: string): Context {
  const d = call.getDeadline();
  const ms = d instanceof Date ? d.getTime() : d;
  const deadline = Number.isFinite(ms) ? performance.now() + (ms - Date.now()) : undefined;
  return new Context({ incoming: fromGrpcMetadata(call.metadata), deadline, method: path });
}

/** "/pkg.Svc/Method" → "Svc.Method", Go's `method` prefix. */
function shortMethod(path: string): string {
  const parts = path.replace(/^\//, "").replace("/", ".").split(".");
  return parts.slice(-2).join(".");
}

/** The status an error thrown by a handler answers with. */
export function toServiceError(e: unknown, path: string, ctx: Context | undefined): Partial<ServiceError> & { code: Status } {
  const withDetails = (code: Status, message: string, details: Uint8Array | undefined) => {
    const metadata = new GrpcMetadata();
    if (details !== undefined) metadata.set(STATUS_DETAILS_KEY, Buffer.from(details));
    return { code, details: message, metadata };
  };
  if (e instanceof UserError) {
    // Go's forCaller: the status message is "<method>: <dev>".
    const prefix = `${shortMethod(path)}: `;
    const msg = e.message.startsWith(prefix) ? e.message : prefix + e.message;
    return withDetails(e.code, msg, encodeRpcStatus({ code: e.code, message: msg, details: [pack(e.detail)] }));
  }
  if (e instanceof StatusError) return withDetails(e.code, e.message, e.statusDetails);
  if (isReturned(e)) return { code: Status.UNKNOWN, details: e.message };
  observability.reportError(ctx, new Error(`PANIC ${path}: ${observability.errorText(e, "stack")}`, { cause: e }));
  return { code: Status.INTERNAL, details: `${path}: internal error` };
}

function startSpan(ctx: Context, path: string): [Context, { end(err?: unknown): void } | undefined] {
  const tr = observability.tracer();
  if (tr === undefined) return [ctx, undefined];
  // Loaded already: observability.init imported it to make the tracer.
  const api = observability.otelApi()!;
  const parent = observability.extractTrace(ctx) ?? api.ROOT_CONTEXT;
  const span = tr.startSpan(
    path,
    { kind: api.SpanKind.SERVER, attributes: { "rpc.system": "grpc", "rpc.method": path } },
    parent,
  );
  return [
    observability.withTrace(ctx, api.trace.setSpan(parent, span)),
    {
      end(err?: unknown) {
        if (err !== undefined) span.setStatus({ code: api.SpanStatusCode.ERROR, message: observability.errorText(err, "json") });
        span.end();
      },
    },
  ];
}

async function guarded(
  call: AnyCall,
  reg: Registry,
  path: string,
  body: (ctx: Context) => Promise<void>,
  fail: (e: Partial<ServiceError>) => void,
): Promise<void> {
  if (reg.draining) {
    // Shutting down: refuse new work with the one code a client retries
    // elsewhere, rather than starting what the drain may cut off.
    fail({ code: Status.UNAVAILABLE, details: "server is shutting down" });
    return;
  }
  const root = contextFor(call, path);
  // grpc-js cancels a call whose deadline passed too: that reads as the
  // deadline, as in Go, not as a client's cancel.
  call.on("cancelled", () => cancelCall(root));
  reg.enter();
  const [ctx, span] = startSpan(root, path);
  try {
    await body(ctx);
    span?.end();
  } catch (e) {
    span?.end(e);
    fail(toServiceError(e, path, ctx));
  } finally {
    reg.leave();
  }
}

async function writeAll(
  call: ServerWritableStream<unknown, unknown> | ServerDuplexStream<unknown, unknown>,
  ctx: Context,
  gen: AsyncIterable<unknown>,
): Promise<void> {
  for await (const resp of gen) {
    if (ctx.done()) break;
    if (!call.write(resp)) {
      await new Promise<void>((r) => {
        const off = ctx.onDone(r);
        call.once("drain", () => {
          off();
          r();
        });
      });
    }
  }
}

/** The status a request stream cut short by the caller reads as (Go's Recv error). */
function cutShort(ctx: Context, cause?: unknown): StatusError {
  const err = ctx.err() ?? "context canceled";
  const code = err === "context deadline exceeded" ? Status.DEADLINE_EXCEEDED : Status.CANCELLED;
  const se = new StatusError(code, err);
  if (cause !== undefined) se.cause = cause;
  return se;
}

const nextMacrotask = () => new Promise<void>((r) => setImmediate(r));

/**
 * @internal The request stream of a client-streaming or bidi call, as Go's
 * Recv reads it: a caller that hangs up (or whose deadline passes)
 * mid-stream is a CANCELLED (DEADLINE_EXCEEDED) StatusError, never the
 * stream's own error — which the server would report as a panic — and never
 * a clean end. Node's http2 ENDS the request stream when the stream is
 * reset, and grpc-js reports the cancel only after that, on the same turn:
 * so an end, or a throw, is read once that turn is over, when the context
 * says which it was. (A grpc-js CLIENT that cancels half-closes first and
 * resets after, in two writes: that end can arrive as a clean one, as it
 * would to a Go server.)
 */
export async function* requests(
  call: ServerReadableStream<unknown, unknown> | ServerDuplexStream<unknown, unknown>,
  ctx: Context,
): AsyncGenerator<unknown> {
  // destroyOnReturn: false — a handler that stops reading (a `break` out of
  // its for-await) only stops reading, as Go's that stops calling Recv. The
  // default return() destroys the whole duplex, the write side with it: the
  // replies and the status would never be sent.
  const it = call.iterator({ destroyOnReturn: false });
  let finished = false;
  try {
    for (;;) {
      let n: IteratorResult<unknown>;
      try {
        n = await it.next();
      } catch (e) {
        finished = true;
        await nextMacrotask();
        throw ctx.done() ? cutShort(ctx, e) : e;
      }
      if (n.done) {
        finished = true;
        await nextMacrotask();
        if (ctx.done()) throw cutShort(ctx);
        return;
      }
      yield n.value;
    }
  } finally {
    // The handler stopped reading early: release the iterator, never the
    // stream (see destroyOnReturn above). What the caller still sends is
    // dropped when the call ends.
    if (!finished) await it.return?.();
  }
}

function handler(m: MethodImpl, reg: Registry): UntypedHandleCall {
  const info = m.info;
  const path = info.path;

  if (!info.clientStreaming && !info.serverStreaming) {
    return ((call: ServerUnaryCall<unknown, unknown>, callback: sendUnaryData<unknown>) => {
      void guarded(
        call,
        reg,
        path,
        async (ctx) => {
          const [c, headers] = paging.withResponseHeaders(ctx);
          const resp = await m.invokeUnary(c, call.request as Message);
          if (headers.size) {
            // The page position a forwarded storage call reported
            // (paging.relayPosition) answers as this method's own.
            const pairs: [string, string][] = [];
            for (const [k, vs] of headers) for (const v of vs) pairs.push([k, v]);
            call.sendMetadata(toGrpcMetadata(pairs));
          }
          callback(null, resp);
        },
        (e) => callback(e, null),
      );
    }) as UntypedHandleCall;
  }

  if (info.serverStreaming && !info.clientStreaming) {
    return ((call: ServerWritableStream<unknown, unknown>) => {
      void guarded(
        call,
        reg,
        path,
        async (ctx) => {
          await writeAll(call, ctx, m.fn(ctx, call.request) as AsyncIterable<unknown>);
          call.end();
        },
        (e) => call.emit("error", e),
      );
    }) as UntypedHandleCall;
  }

  if (info.clientStreaming && !info.serverStreaming) {
    return ((call: ServerReadableStream<unknown, unknown>, callback: sendUnaryData<unknown>) => {
      void guarded(
        call,
        reg,
        path,
        async (ctx) => {
          const resp = (await m.fn(ctx, requests(call, ctx))) as unknown;
          if (resp === undefined || resp === null) throw new StatusError(Status.INTERNAL, `${path}: handler returned no response`);
          callback(null, resp);
        },
        (e) => callback(e, null),
      );
    }) as UntypedHandleCall;
  }

  return ((call: ServerDuplexStream<unknown, unknown>) => {
    void guarded(
      call,
      reg,
      path,
      async (ctx) => {
        await writeAll(call, ctx, m.fn(ctx, requests(call, ctx)) as AsyncIterable<unknown>);
        call.end();
      },
      (e) => call.emit("error", e),
    );
  }) as UntypedHandleCall;
}
