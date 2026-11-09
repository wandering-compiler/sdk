/**
 * The business tier's lifecycle: boot from the manifest, serve, drain.
 *
 * `node dist/main.js serve --manifest w17-business.json --project-root DIR`
 * does what Go's generated business `main` does:
 *
 * 1. read the manifest, load `descriptor_set` into the pool;
 * 2. the config (a value that does not parse, or a required key with no
 *    value, stops the boot naming it);
 * 3. Sentry and OpenTelemetry from `W17_SENTRY_DSN` / `W17_OTEL_*`;
 * 4. the buses: NATS and/or Redis Streams, each DSN resolved through the
 *    secrets chain; a bus whose DSN is empty is off and its events are
 *    dropped, as Go leaves the dispatcher nil;
 * 5. the storage clients (one connection, dialled lazily);
 * 6. import `<project-root>/<register.path>/<register.module>` (`w17.ts`, run
 *    by Node's type stripping) and call `registerBusiness(cfg, registry,
 *    clients)` (sync or async); every method a plugin hook is bound to must
 *    then have a handler;
 * 7. serve h2c, or TLS with `W17_INTERNAL_TLS`; `grpc.health.v1`; reflection
 *    when `<PREFIX>_GRPC_REFLECTION` is `1`/`true`;
 * 8. on SIGTERM/SIGINT: report NOT_SERVING, refuse new calls, give the
 *    in-flight ones `shutdown_timeout_seconds` (10, Go's ShutdownTimeout),
 *    cancel the rest and give them one more timeout to return (a rollback,
 *    a commit in progress), wait for the work committed transactions still owe
 *    (events, onCommit hooks), then run the shutdown hooks under one 30s
 *    drain deadline (Go's shutdownDrainTimeout), close the buses and the
 *    channels, flush telemetry.
 */

import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { Server, type ServerOptions } from "@grpc/grpc-js";
import { Conn, ServiceClient, TxCoordinator, parseAddr } from "./client.js";
import { Config, ConfigError } from "./config.js";
import { Context } from "./context.js";
import { type Pool, getDefaultPool } from "./descriptors.js";
import * as events from "./events.js";
import { HealthService } from "./health.js";
import { type Manifest, emitsOf, invalidatesOf, load, manifestPath } from "./manifest.js";
import * as observability from "./observability.js";
import * as principal from "./principal.js";
import * as reflection from "./reflection.js";
import { type Env, newResolver } from "./secrets.js";
import { RegistrationError, Registry } from "./server.js";
import * as tlsconfig from "./tlsconfig.js";
import * as txscope from "./txscope.js";

/** lifecycle.DefaultShutdownTimeout; the manifest's shutdown_timeout_seconds wins. */
export const SHUTDOWN_TIMEOUT_MS = 10_000;
/**
 * runtime.shutdownDrainTimeout: the budget of the whole drain — the server's
 * stop, the handlers and the work after a commit, the shutdown hooks and the
 * bus close — unless the shutdown timeout is longer.
 */
export const DRAIN_TIMEOUT_MS = 30_000;
export const ENV_MANIFEST = "W17_BUSINESS_MANIFEST";
export const ENV_PROJECT_ROOT = "W17_BUSINESS_PROJECT_ROOT";

/** runtime.serverOptions: ping an idle client every 30s, 10s to answer. */
export const SERVER_OPTIONS: ServerOptions = {
  "grpc.keepalive_time_ms": 30_000,
  "grpc.keepalive_timeout_ms": 10_000,
  "grpc.keepalive_permit_without_calls": 1,
};

/** The tier cannot start; the message names what to fix. */
export class BootError extends Error {
  override name = "BootError";
}

const msgOf = (e: unknown) => observability.errorText(e, "message");

/**
 * The tier's clients, built from the manifest's `storage` section.
 *
 * `clients.service("shop.core.OrderQuery")` (or the short name when
 * unambiguous) returns a `ServiceClient`; `clients.distributedTransaction()`
 * returns the coordinator `distx` drives. One connection serves them all.
 */
export class Clients {
  readonly #conn: Conn | undefined;
  readonly #byService = new Map<string, ServiceClient>();
  readonly #tx: TxCoordinator | undefined;

  constructor(pool: Pool, manifest: Manifest, environ: Env = process.env) {
    const tier = manifest.storage;
    if (tier === null) return;
    const addr = environ[tier.addressEnv] || tier.defaultAddress;
    if (!addr) throw new BootError(`storage: ${tier.addressEnv} is not set and the manifest gives no default address`);
    try {
      parseAddr(addr);
    } catch (e) {
      throw new BootError(`storage: ${tier.addressEnv}=${JSON.stringify(addr)}: ${msgOf(e)}`);
    }
    let conn: Conn;
    try {
      conn = new Conn(addr, { environ });
    } catch (e) {
      throw new BootError(`storage tls: ${msgOf(e)}`);
    }
    this.#conn = conn;
    for (const svc of tier.services) {
      let full: string;
      try {
        full = pool.service(svc).typeName;
      } catch {
        throw new BootError(`storage: service ${JSON.stringify(svc)} is not in the descriptor set`);
      }
      this.#byService.set(full, new ServiceClient(conn, full, pool));
    }
    if (tier.distributedTransactions) this.#tx = new TxCoordinator(conn);
  }

  service(name: string): ServiceClient {
    const n = name.replace(/^\./, "");
    const exact = this.#byService.get(n);
    if (exact !== undefined) return exact;
    const matches = [...this.#byService.keys()].filter((s) => s.slice(s.lastIndexOf(".") + 1) === n);
    if (matches.length === 1) return this.#byService.get(matches[0]!)!;
    if (!matches.length) {
      throw new RangeError(`clients: no storage service ${JSON.stringify(n)} (have: ${[...this.#byService.keys()].sort().join(", ")})`);
    }
    throw new RangeError(`clients: ${JSON.stringify(n)} is ambiguous (${matches.sort().join(", ")})`);
  }

  distributedTransaction(): TxCoordinator {
    if (this.#tx === undefined) {
      throw new Error(
        "clients: this domain's storage registers no distributed-transaction coordinator " +
          "(it declares no mutation service), so there is no transaction to begin",
      );
    }
    return this.#tx;
  }

  close(): void {
    this.#conn?.close();
  }
}

/** One business tier, booted from a manifest. `run` serves it. */
export class App {
  readonly manifest: Manifest;
  readonly env: Env;
  readonly projectRoot: string;
  readonly pool: Pool;
  obs: observability.Observability | undefined;
  cfg: Config | undefined;
  clients: Clients | undefined;
  registry: Registry | undefined;
  dispatcher: events.Dispatcher | undefined;
  server: Server | undefined;
  readonly health = new HealthService();
  addr = "";
  port = 0;
  /** @internal The drain's budget ({@link DRAIN_TIMEOUT_MS}); tests shorten it. */
  readonly drainTimeoutMs: number;

  constructor(manifest: Manifest, opts: { environ?: Env; projectRoot?: string; drainTimeoutMs?: number } = {}) {
    this.manifest = manifest;
    this.drainTimeoutMs = opts.drainTimeoutMs ?? DRAIN_TIMEOUT_MS;
    this.env = { ...(opts.environ ?? process.env) };
    this.projectRoot = opts.projectRoot || this.env[ENV_PROJECT_ROOT] || process.cwd();
    this.pool = getDefaultPool();
  }

  // --- boot ----------------------------------------------------------------

  async boot(): Promise<void> {
    const m = this.manifest;
    const set = manifestPath(m, m.descriptorSet);
    try {
      this.pool.addSetFile(set);
    } catch (e) {
      throw new BootError(`descriptor_set: ${set}: ${msgOf(e)}`);
    }
    this.#authResp();
    try {
      this.cfg = await Config.load(m.config, this.env);
    } catch (e) {
      if (e instanceof ConfigError) throw new BootError(e.message);
      throw e;
    }
    try {
      this.obs = await observability.init(m.bundle, { scrubFields: this.cfg.secretEnvNames(), environ: this.env });
    } catch (e) {
      throw new BootError(`observability: ${msgOf(e)}`);
    }
    this.dispatcher = await this.#bus();
    this.clients = new Clients(this.pool, m, this.env);
    this.registry = new Registry(this.pool, {
      emits: emitsOf(m),
      dispatcher: this.dispatcher,
      invalidates: invalidatesOf(m),
      services: m.services.map((s) => s.name),
    });
    this.#checkEvents();
    await this.#register();
  }

  /** The project's AuthResp, for principal.userId's envelope fallback: the one message of that name. */
  #authResp(): void {
    const found = this.pool.findMessagesNamed("AuthResp");
    if (found.length === 1) principal.setAuthRespType(this.pool.schema(found[0]!));
  }

  #checkEvents(): void {
    for (const e of emitsOf(this.manifest)) {
      try {
        new events.EnvelopeBuilder(this.pool, e);
      } catch (err) {
        throw new BootError(`events: ${e.method}: ${msgOf(err)}`);
      }
    }
  }

  async #bus(): Promise<events.Dispatcher | undefined> {
    const ev = this.manifest.events;
    if (ev === null || !(ev.natsDsnEnv || ev.redisDsnEnv)) return undefined;
    let res;
    try {
      res = await newResolver(this.env);
    } catch (e) {
      throw new BootError(`secrets: ${msgOf(e)}`);
    }
    let nats: events.Dispatcher | undefined;
    let redis: events.Dispatcher | undefined;
    try {
      const dsn = ev.natsDsnEnv ? res.get(ev.natsDsnEnv) : "";
      if (dsn) nats = await events.NatsPublisher.connect(dsn);
    } catch (e) {
      throw new BootError(`eventbus nats: ${ev.natsDsnEnv}: ${msgOf(e)}`);
    }
    try {
      const dsn = ev.redisDsnEnv ? res.get(ev.redisDsnEnv) : "";
      if (dsn) redis = await events.RedisPublisher.connect(dsn);
    } catch (e) {
      throw new BootError(`eventbus redis: ${ev.redisDsnEnv}: ${msgOf(e)}`);
    }
    if (nats === undefined && redis === undefined) return undefined;
    return new events.Router(nats, redis, ev.redisChannels);
  }

  async #register(): Promise<void> {
    const m = this.manifest;
    const reg = m.register;
    const file = resolve(this.projectRoot, reg.path, reg.module);
    let mod: Record<string, unknown>;
    try {
      mod = (await import(pathToFileURL(file).href)) as Record<string, unknown>;
    } catch (e) {
      throw new BootError(`register: import ${reg.path}/${reg.module} (${file}): ${msgOf(e)}`);
    }
    const fn = mod[reg.function];
    if (typeof fn !== "function") throw new BootError(`register: ${reg.path}/${reg.module} exports no ${reg.function}(cfg, registry, clients)`);
    try {
      await (fn as (...a: unknown[]) => unknown)(this.cfg, this.registry, this.clients);
    } catch (e) {
      if (e instanceof RegistrationError) throw new BootError(e.message);
      throw e;
    }
    const served = new Set(this.registry!.served.flatMap((s) => s.paths()));
    for (const hook of m.pluginHooks) {
      for (const path of hook.methods) {
        if (!served.has(path)) {
          throw new BootError(
            `plugin hook ${hook.plugin}/${hook.slot} (${hook.registeredAs}) calls ${path}, which has no ` +
              "handler: register its service, or the plugin's call fails at runtime",
          );
        }
      }
    }
    const names = new Set(this.registry!.served.map((s) => s.service));
    for (const svc of m.services) {
      if (!names.has(svc.name)) observability.logger.info(`${svc.name} is not registered; its calls answer Unimplemented, as in Go`);
    }
  }

  // --- serve ---------------------------------------------------------------

  listenAddr(): string {
    return this.env[this.manifest.listenEnv] || this.manifest.defaultListenAddr;
  }

  async start(): Promise<void> {
    const reg = this.registry!;
    let tls: tlsconfig.ServerTLS;
    try {
      tls = tlsconfig.serverTLS(this.env);
    } catch (e) {
      throw new BootError(`server tls: ${msgOf(e)}`);
    }
    const listen = this.listenAddr();
    let host: string;
    let port: number;
    try {
      [host, port] = parseAddr(listen);
    } catch (e) {
      throw new BootError(`${this.manifest.listenEnv}=${JSON.stringify(listen)}: ${msgOf(e)}`);
    }
    // ":50051" listens on every interface, as in Go.
    if (listen.startsWith(":")) host = "0.0.0.0";
    const server = new Server(SERVER_OPTIONS);
    reg.addTo(server);
    this.health.addTo(server);
    const v = this.env[this.manifest.reflectionEnv] ?? "";
    if (v === "1" || v === "true") {
      const names = () => [...reg.served.map((s) => s.service), "grpc.health.v1.Health", ...reflection.SERVICES];
      reflection.addTo(server, this.pool, names);
    }
    const bindHost = host.includes(":") ? `[${host}]` : host;
    this.port = await new Promise<number>((res, rej) => {
      server.bindAsync(`${bindHost}:${port}`, tlsconfig.serverCredentials(tls), (err, p) => (err ? rej(new BootError(`listen ${listen}: ${err.message}`)) : res(p)));
    });
    this.server = server;
    this.addr = `${host}:${this.port}`;
    this.health.set(true);
    observability.logger.info(`${this.manifest.bundle} listening on ${this.addr}${tls.tls ? " (TLS)" : ""}`);
  }

  async shutdown(): Promise<void> {
    const reg = this.registry;
    const srv = this.server;
    const timeout = this.manifest.shutdownTimeoutMs || SHUTDOWN_TIMEOUT_MS;
    observability.logger.info(`${this.manifest.bundle}: shutting down`);
    // One budget for the whole drain, set once, now: Go's shutdown drain
    // timeout, or the operator's shutdown timeout when that is longer. Every
    // step below draws from what is left of it, so a deployment's
    // termination grace bounds the drain as a whole, not each step alone.
    const drain = new Context().withTimeout(Math.max(this.drainTimeoutMs, timeout));
    const left = () => Math.max(drain.timeRemaining() ?? 0, 0);
    this.health.set(false);
    if (reg !== undefined) reg.draining = true;
    if (srv !== undefined) {
      // Go's GracefulStop bounded by the timeout, then Stop: no new calls,
      // the in-flight ones finish, then whatever is left is cancelled.
      const stopMs = Math.min(timeout, left());
      let timer: NodeJS.Timeout | undefined;
      const graceful = await Promise.race([
        new Promise<boolean>((res) => srv.tryShutdown(() => res(true))),
        new Promise<boolean>((res) => {
          timer = setTimeout(() => res(false), stopMs);
        }),
      ]);
      clearTimeout(timer);
      if (!graceful) {
        observability.reportError(undefined, `runtime: gRPC handler(s) still running after ${stopMs}ms; cancelling them`);
        srv.forceShutdown();
      }
    }
    // After the server's close, one more timeout (within the budget),
    // computed once and SHARED by the two waits below:
    //  - the handlers it cancelled: the server's close does not wait for
    //    them, and one still running (a detached rollback, a commit) needs
    //    the storage channel and the bus it would close under it;
    //  - the work a committed transaction (or an emit) owes, which outlives
    //    its request. Waited for only after the handlers: one cancelled may
    //    have committed and started that work just then.
    // What is still running at the deadline is abandoned and reported, and
    // only then does the bus close under it.
    const afterStop = performance.now() + Math.min(timeout, left());
    const untilAfterStop = () => Math.max(afterStop - performance.now(), 0);
    if (reg !== undefined && !(await reg.waitIdle(untilAfterStop()))) {
      observability.reportError(undefined, `runtime: ${reg.inflight} gRPC handler(s) still running after the server closed; continuing without them`);
    }
    if (!(await txscope.drain(untilAfterStop()))) {
      const n = txscope.abandonPending();
      observability.reportError(
        undefined,
        `runtime: ${n} piece(s) of work after a commit (events, onCommit hooks) still running at the shutdown deadline; abandoned`,
      );
    }
    // The shutdown hooks and the bus close get the rest of the budget.
    const remaining = () => Math.max(left(), 1);
    for (const [i, fn] of (reg?.shutdowns ?? []).entries()) {
      try {
        await bounded(Promise.resolve().then(() => fn(drain)), remaining(), `shutdown hook ${i}`);
      } catch (e) {
        observability.reportError(drain, new Error(`runtime: shutdown hook ${i}: ${msgOf(e)}`));
      }
    }
    if (this.dispatcher !== undefined) {
      try {
        await bounded(this.dispatcher.close(), remaining(), "eventbus close");
      } catch (e) {
        observability.logger.warn(`eventbus close: ${msgOf(e)}`);
      }
    }
    this.clients?.close();
    await this.obs?.shutdown();
  }

  /** Boot, serve until `stop` resolves (default: SIGTERM/SIGINT), then drain. */
  async run(stop?: Promise<unknown>): Promise<void> {
    await this.boot();
    await this.start();
    let onSignal: (() => void) | undefined;
    const signalled = new Promise<void>((res) => {
      onSignal = res;
      process.once("SIGTERM", res);
      process.once("SIGINT", res);
    });
    try {
      await (stop ?? signalled);
    } finally {
      process.removeListener("SIGTERM", onSignal!);
      process.removeListener("SIGINT", onSignal!);
      await this.shutdown();
    }
  }
}

function bounded<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  return Promise.race([
    p,
    new Promise<never>((_, rej) => {
      timer = setTimeout(() => rej(new Error(`${what}: timed out after ${ms}ms`)), ms);
    }),
  ]).finally(() => clearTimeout(timer));
}

/** The manifest path: the flag, else `$W17_BUSINESS_MANIFEST`, else ./w17-business.json. */
export function resolveManifestPath(arg: string | undefined, environ: Env = process.env): string {
  return arg || environ[ENV_MANIFEST] || "w17-business.json";
}

/** Boot and serve until SIGTERM/SIGINT. */
export async function serve(manifest?: string, projectRoot?: string): Promise<void> {
  const m = load(resolveManifestPath(manifest));
  await new App(m, projectRoot !== undefined ? { projectRoot } : {}).run();
}
