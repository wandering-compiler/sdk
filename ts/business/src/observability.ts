/**
 * Sentry and OpenTelemetry, configured as Go's `core/observx` configures them.
 *
 * - `W17_SENTRY_DSN` turns Sentry on (traces sampled at 0.2, server name =
 *   the service, release = `W17_SERVICE_VERSION`, environment = `W17_ENV`).
 * - `W17_OTEL_ENDPOINT` (fallback `OTEL_EXPORTER_OTLP_ENDPOINT`) turns
 *   OTLP/gRPC trace export on; `OTEL_SERVICE_NAME` overrides the service name.
 * - `W17_OTEL_TLS` — on|true|1|yes / off|false|0|no; empty follows the
 *   internal mesh (`W17_INTERNAL_TLS`), with its CA and client pair.
 * - W3C trace context (`traceparent`/`tracestate`) and baggage are the
 *   propagators, installed only when OTel export is on — as in Go, where the
 *   global propagator stays the no-op one otherwise.
 *
 * The trace context travels on the SDK's explicit Context, never through an
 * ambient context manager: the server puts its span's OTel context on the
 * request's ctx, and a client call injects from the ctx it is given.
 *
 * `reportError` is `observx.ReportError`: Sentry when on, the log always.
 */

import type { Context } from "./context.js";
import { isCredentialKey } from "./credentials.js";
import * as tlsconfig from "./tlsconfig.js";
import type { Env } from "./secrets.js";

type OtelApi = typeof import("@opentelemetry/api");
type Sentry = typeof import("@sentry/node");

interface State {
  sentry: Sentry | undefined;
  otel: OtelApi | undefined;
  tracer: import("@opentelemetry/api").Tracer | undefined;
  provider: { shutdown(): Promise<void>; forceFlush(): Promise<void> } | undefined;
}

const state: State = { sentry: undefined, otel: undefined, tracer: undefined, provider: undefined };

const OTEL_CONTEXT = Symbol("w17.otel.context");

function log(level: "error" | "warn" | "info", text: string): void {
  const line = `${new Date().toISOString()} ${level.toUpperCase()} w17_business: ${text}`;
  if (level === "info") console.log(line);
  else console.error(line);
}

export const logger = {
  error: (t: string) => log("error", t),
  warn: (t: string) => log("warn", t),
  info: (t: string) => log("info", t),
};

/**
 * Report an error an operator must see, without failing anything.
 *
 * Mirrors observx.ReportError: to Sentry when it is on, and to the log
 * always (Go falls through to log.Printf without Sentry).
 */
export function reportError(_ctx: Context | undefined, err: unknown): void {
  const text = typeof err === "string" ? err : err instanceof Error ? `${err.name}: ${err.message}` : String(err);
  logger.error(`error: ${text}`);
  const s = state.sentry;
  if (s !== undefined) {
    if (typeof err === "string") s.captureMessage(err, "error");
    else s.captureException(err);
  }
}

const truthy = (v: string) => ["on", "true", "1", "yes"].includes(v.trim().toLowerCase());
const falsy = (v: string) => ["off", "false", "0", "no"].includes(v.trim().toLowerCase());

/** What `init` turned on; `shutdown` flushes and stops it. */
export class Observability {
  readonly sentry: boolean;
  readonly otel: boolean;
  constructor(sentry: boolean, otel: boolean) {
    this.sentry = sentry;
    this.otel = otel;
  }

  async shutdown(timeoutMs = 5000): Promise<void> {
    const s = state.sentry;
    if (s !== undefined) {
      try {
        await s.flush(timeoutMs);
      } catch (e) {
        logger.warn(`sentry flush: ${String(e)}`);
      }
    }
    const p = state.provider;
    if (p !== undefined) {
      try {
        await p.shutdown();
      } catch (e) {
        logger.warn(`otel shutdown: ${String(e)}`);
      }
    }
    state.sentry = state.otel = state.tracer = state.provider = undefined;
  }
}

const SCRUB = "[scrubbed]";

function escapeRe(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * Redact credentials and the given field names from a Sentry event.
 *
 * The request headers a credential rides in are dropped whatever the
 * project declares; `scrubFields` (the secret config keys' variables) are
 * redacted wherever they appear as a key in request data or frame vars, or
 * as `name=value` inside a string (an error message, a log line).
 */
 
export function scrubEvent<T extends Record<string, any>>(event: T, scrubFields: Iterable<string> = []): T {
  const fields = new Set(scrubFields);
  const patterns = [...fields].map(
    (f) => new RegExp(`(['"]?\\b${escapeRe(f)}\\b['"]?\\s*[=:]\\s*)('[^']*'|"[^"]*"|[^\\s,)\\]}]+)`, "gi"),
  );
  const scrub = (v: unknown): unknown => {
    if (Array.isArray(v)) return v.map(scrub);
    if (v !== null && typeof v === "object") {
      const o = v as Record<string, unknown>;
      for (const k of Object.keys(o)) {
        if (fields.has(k) || isCredentialKey(k)) o[k] = SCRUB;
        else o[k] = scrub(o[k]);
      }
      return o;
    }
    if (typeof v === "string") {
      let s = v;
      for (const p of patterns) s = s.replace(p, (_m, pre: string) => pre + SCRUB);
      return s;
    }
    return v;
  };
   
  const e = event as Record<string, any>;
  const req = e.request;
  if (req && typeof req === "object") {
    for (const key of ["headers", "data", "cookies"]) if (key in req) req[key] = scrub(req[key]);
  }
  for (const container of ["exception", "threads"]) {
    const values = e[container]?.values;
    if (!Array.isArray(values)) continue;
    for (const value of values) {
      if (!value || typeof value !== "object") continue;
      if (typeof value.value === "string") value.value = scrub(value.value);
      const frames = value.stacktrace?.frames;
      if (Array.isArray(frames)) for (const fr of frames) if (fr?.vars && typeof fr.vars === "object") fr.vars = scrub(fr.vars);
    }
  }
  if (typeof e.message === "string") e.message = scrub(e.message);
  const entry = e.logentry;
  if (entry && typeof entry === "object") {
    for (const key of ["message", "formatted"]) if (typeof entry[key] === "string") entry[key] = scrub(entry[key]);
    if (Array.isArray(entry.params)) entry.params = scrub(entry.params);
  }
  if (e.extra && typeof e.extra === "object") e.extra = scrub(e.extra);
  return event;
}

/**
 * Turn Sentry and OpenTelemetry on per the environment.
 *
 * A bad value (an unknown `W17_OTEL_TLS`) throws: as in Go, the only valid
 * recovery from a bad telemetry config is to fix it and restart.
 */
export async function init(
  serviceName: string,
  opts: { scrubFields?: Iterable<string>; environ?: Env } = {},
): Promise<Observability> {
  const env = opts.environ ?? process.env;
  const service = env.OTEL_SERVICE_NAME || serviceName;
  const version = env.W17_SERVICE_VERSION ?? "";
  const environment = env.W17_ENV ?? "";
  let sentryOn = false;
  let otelOn = false;

  const endpoint = env.W17_OTEL_ENDPOINT || env.OTEL_EXPORTER_OTLP_ENDPOINT || "";
  let secure = false;
  if (endpoint) {
    const otelTLS = env.W17_OTEL_TLS ?? "";
    if (otelTLS && !(truthy(otelTLS) || falsy(otelTLS))) {
      throw new RangeError(`W17_OTEL_TLS=${JSON.stringify(otelTLS)} is not a recognised value (on|true|1|yes, off|false|0|no)`);
    }
    const mesh = env[tlsconfig.ENV_INTERNAL_TLS];
    tlsconfig.check(mesh);
    secure = otelTLS ? truthy(otelTLS) : tlsconfig.dialTLS(mesh);
    if (otelTLS && falsy(otelTLS) && tlsconfig.dialTLS(mesh)) {
      logger.warn(`observability: W17_OTEL_TLS=${otelTLS} exports telemetry in plaintext while ${tlsconfig.ENV_INTERNAL_TLS} is on`);
    }
  }

  const dsn = env.W17_SENTRY_DSN ?? "";
  if (dsn) {
    const Sentry = await import("@sentry/node");
    const fields = [...(opts.scrubFields ?? [])];
    Sentry.init({
      dsn,
      serverName: service,
      ...(version && { release: version }),
      ...(environment && { environment }),
      tracesSampleRate: 0.2,
      // Go's Sentry client sends no request bodies, cookies or IPs; keep this
      // one equally quiet.
      dataCollection: { userInfo: false, cookies: false, httpBodies: [], urlQueryParams: false },
      // Go's events carry no stack-frame variables; a frame's locals hold
      // requests, configs and secrets — send none.
      includeLocalVariables: false,
      // The SDK runs its own OpenTelemetry pipeline (below); Sentry must not
      // register a competing global provider.
      enableOpenTelemetrySetup: false,
      beforeSend: (event) => scrubEvent(event, fields),
    });
    state.sentry = Sentry;
    sentryOn = true;
  }

  if (endpoint) {
    const api = await import("@opentelemetry/api");
    const core = await import("@opentelemetry/core");
    const { resourceFromAttributes } = await import("@opentelemetry/resources");
    const sdk = await import("@opentelemetry/sdk-trace");
    const { OTLPTraceExporter } = await import("@opentelemetry/exporter-trace-otlp-grpc");
    const exporter = new OTLPTraceExporter({
      url: /^[a-z]+:\/\//.test(endpoint) ? endpoint : `${secure ? "https" : "http"}://${endpoint}`,
      credentials: secure ? tlsconfig.otlpCredentials(env) : (await import("@grpc/grpc-js")).credentials.createInsecure(),
    });
    const provider = new sdk.TracerProvider({
      resource: resourceFromAttributes({
        "service.name": service,
        "service.version": version,
        "deployment.environment.name": environment,
      }),
      sampler: new sdk.ParentBasedSampler({ root: new sdk.TraceIdRatioBasedSampler(1.0) }),
      spanProcessors: [new sdk.BatchSpanProcessor({ exporter })],
    });
    api.trace.setGlobalTracerProvider(provider);
    api.propagation.setGlobalPropagator(
      new core.CompositePropagator({
        propagators: [new core.W3CTraceContextPropagator(), new core.W3CBaggagePropagator()],
      }),
    );
    state.provider = provider;
    state.otel = api;
    state.tracer = api.trace.getTracer("w17_business");
    otelOn = true;
  }
  return new Observability(sentryOn, otelOn);
}

/** The SDK's tracer, or undefined when OTel export is off. */
export function tracer(): import("@opentelemetry/api").Tracer | undefined {
  return state.tracer;
}

/** The OTel context the incoming metadata names (traceparent), or undefined. */
export function extractTrace(ctx: Context): import("@opentelemetry/api").Context | undefined {
  const api = state.otel;
  if (api === undefined) return undefined;
  const carrier: Record<string, string> = {};
  for (const [k, v] of ctx.incoming) if (v[0] !== undefined) carrier[k] = v[0];
  return api.propagation.extract(api.ROOT_CONTEXT, carrier);
}

/** ctx carrying an OTel context (the server's span), for the calls made with it. */
export function withTrace(ctx: Context, otelCtx: import("@opentelemetry/api").Context): Context {
  return ctx.withValue(OTEL_CONTEXT, otelCtx);
}

/** Add the trace context ctx carries to outgoing metadata pairs. */
export function injectTrace(ctx: Context, pairs: [string, string][]): [string, string][] {
  const api = state.otel;
  if (api === undefined) return pairs;
  const otelCtx = ctx.value<import("@opentelemetry/api").Context>(OTEL_CONTEXT);
  if (otelCtx === undefined) return pairs;
  const carrier: Record<string, string> = {};
  api.propagation.inject(otelCtx, carrier);
  const keys = Object.keys(carrier);
  if (!keys.length) return pairs;
  return [...pairs.filter(([k]) => !keys.includes(k)), ...Object.entries(carrier)];
}

/** The OpenTelemetry API module, loaded when export is on. */
export function otelApi(): OtelApi | undefined {
  return state.otel;
}
