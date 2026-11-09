/**
 * The business tier's manifest (`w17-business.json`), formats 1 and 2.
 *
 * Codegen writes it beside `descriptors.binpb` in
 * `w17/services/<domain>-business/`; the SDK reads it at boot. The platform's
 * spec `bundles/business-manifest` is the contract; this module is its reader:
 *
 * - `format` is checked first, and a format this SDK does not know is refused
 *   naming the ones it reads;
 * - format 2 is format 1 plus the `companion` section (egress clients and file
 *   storage served by the business companion); a manifest whose `companion`
 *   is null stays format 1, and a format-1 manifest carrying one is refused;
 * - a field is never repurposed, so unknown keys are ignored (kept in `extra`);
 * - `null` (or an empty list) means "this domain has none".
 *
 * Every value is what the Go business bundle of the same domain uses: the same
 * variables, the same methods, the same storage services, the same events.
 */

import { readFileSync } from "node:fs";
import { dirname, isAbsolute, join, normalize, basename } from "node:path";

export const SUPPORTED_FORMATS: readonly number[] = [1, 2];
/** The highest format this SDK reads. */
export const SUPPORTED_FORMAT = 2;
export const LANGUAGE = "typescript";

export class ManifestError extends Error {
  override name = "ManifestError";
}

/**
 * Where the author's code is: `path` is the project-relative directory of the
 * author's package; the SDK imports `<path>/<module>` (`w17.ts`) and calls
 * `function` (`registerBusiness`).
 */
export interface Register {
  readonly path: string;
  readonly package: string;
  readonly module: string;
  readonly function: string;
}

export interface MethodSpec {
  readonly name: string;
  readonly path: string;
  readonly input: string;
  readonly output: string;
  readonly clientStreaming: boolean;
  readonly serverStreaming: boolean;
}

export interface ServiceSpec {
  readonly name: string;
  readonly methods: readonly MethodSpec[];
}

export type ConfigType = "string" | "bool" | "int";

export interface ConfigKey {
  readonly name: string;
  readonly env: string;
  readonly type: string;
  /** Normalised to the text a variable would hold. */
  readonly default: string;
  readonly required: boolean;
  readonly secret: boolean;
  readonly description: string;
}

export interface StorageTier {
  readonly addressEnv: string;
  readonly defaultAddress: string;
  readonly services: readonly string[];
  /** The coordinator's full service name, "" when the storage bundle registers none. */
  readonly distributedTransactions: string;
}

/** One event field and where its value comes from. */
export interface PayloadBinding {
  readonly field: string;
  /** request | response | literal */
  readonly source: string;
  readonly path: readonly string[];
  /** For literal. */
  readonly value: string;
}

/** A method's `(w17.event_emit)`. */
export interface Emit {
  /** The gRPC path, "/pkg.Svc/Method". */
  readonly method: string;
  /** The event message's full name. */
  readonly event: string;
  /** The envelope message's full name (events.envelope). */
  readonly envelope: string;
  /** The envelope's payload case for this event (proto field name). */
  readonly envelopeField: string;
  readonly channel: string;
  readonly topic: string;
  readonly fields: readonly PayloadBinding[];
}

/**
 * A method's `(w17.touches_entities)`: one `w17.InvalidateEvent` per entity,
 * published after the method's emits.
 */
export interface Invalidate {
  readonly method: string;
  readonly channel: string;
  readonly topic: string;
  readonly group: string;
  readonly entities: readonly string[];
}

export interface Events {
  /** "" when the project runs no NATS. */
  readonly natsDsnEnv: string;
  /** "" when no channel is on Redis Streams. */
  readonly redisDsnEnv: string;
  readonly redisChannels: readonly string[];
  readonly durablePrefixEnv: string;
  readonly durablePrefixDefault: string;
  readonly envelope: string;
  readonly emits: readonly Emit[];
  readonly invalidates: readonly Invalidate[];
}

export interface PluginHook {
  readonly plugin: string;
  readonly registeredAs: string;
  readonly slot: string;
  /** gRPC paths. */
  readonly methods: readonly string[];
}

/** One egress client the companion serves (`proto/clients/<client>/`). */
export interface EgressClient {
  /** "quotes" */
  readonly client: string;
  /** "clients.quotes.QuotesApi", in the descriptor set. */
  readonly service: string;
  /** "W17_CLIENT_QUOTES_DSN" (read by the companion, not here). */
  readonly dsnEnv: string;
}

/** One upload connection an `UPLOADED_*` field of the domain names. */
export interface FileConnection {
  readonly name: string;
  /** Read by the companion, not here. */
  readonly rootEnv: string;
  readonly defaultRoot: string;
  /** The business tier writes it (`upload.writer: BUSINESS`). */
  readonly writable: boolean;
}

/** One `writer: BUSINESS` table field: where its files go. */
export interface FilePlacement {
  /** Full proto name, "app.example.NoteExport.file". */
  readonly field: string;
  /** "NoteExportFile", the Go identifier stem. */
  readonly name: string;
  readonly connection: string;
  readonly bucketPath: string;
  readonly expectedObjects: bigint;
  readonly maxPerBucket: number;
  /** 0n = no cap. */
  readonly maxSizeBytes: bigint;
}

export interface FileStorage {
  readonly service: string;
  readonly connections: readonly FileConnection[];
  readonly placements: readonly FilePlacement[];
}

/**
 * The business companion: the Go process that serves this tier the project's
 * egress clients and the domain's file stores over gRPC.
 */
export interface Companion {
  readonly addressEnv: string;
  readonly defaultAddress: string;
  readonly egress: readonly EgressClient[];
  readonly files: FileStorage | null;
}

export interface Manifest {
  readonly format: number;
  readonly domain: string;
  readonly bundle: string;
  readonly language: string;
  readonly sdkVersion: string;
  readonly envPrefix: string;
  readonly listenEnv: string;
  readonly defaultListenAddr: string;
  readonly reflectionEnv: string;
  /** Milliseconds (the manifest says seconds). */
  readonly shutdownTimeoutMs: number;
  /** Relative to the manifest. */
  readonly descriptorSet: string;
  readonly register: Register;
  readonly services: readonly ServiceSpec[];
  readonly config: readonly ConfigKey[];
  readonly storage: StorageTier | null;
  readonly events: Events | null;
  readonly pluginHooks: readonly PluginHook[];
  readonly companion: Companion | null;
  readonly baseDir: string;
  readonly extra: Readonly<Record<string, unknown>>;
}

/** A path of the manifest, resolved against its directory. */
export function manifestPath(m: Manifest, rel: string): string {
  return isAbsolute(rel) ? rel : normalize(join(m.baseDir, rel));
}

export function emitsOf(m: Manifest): readonly Emit[] {
  return m.events?.emits ?? [];
}

export function invalidatesOf(m: Manifest): readonly Invalidate[] {
  return m.events?.invalidates ?? [];
}

const TOP = new Set([
  "format", "domain", "bundle", "language", "sdk_version", "env_prefix", "listen", "reflection_env",
  "shutdown_timeout_seconds", "descriptor_set", "register", "services", "config", "storage", "events",
  "plugin_hooks", "companion",
]);

type Obj = Record<string, unknown>;

function isObj(v: unknown): v is Obj {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** JSON.stringify, writing a bigint (an integer {@link parseJSON} kept exact) as the number it was. */
function json(v: unknown): string {
  const j = JSON as unknown as { rawJSON(s: string): unknown };
  return JSON.stringify(v, (_k, x: unknown) => (typeof x === "bigint" ? j.rawJSON(x.toString()) : x));
}

/** A JSON scalar as text; anything else as its JSON. */
function text(v: unknown): string {
  return typeof v === "string" ? v : typeof v === "number" || typeof v === "boolean" || typeof v === "bigint" ? String(v) : json(v);
}

/**
 * JSON.parse keeping an integer beyond 2^53 exact, as a bigint: a number
 * would round `9007199254740993` to `...992`, and Python's json (and Go's
 * config loader) read it exactly.
 */
export function parseJSON(src: string): unknown {
  const reviver = (_k: string, v: unknown, c?: { source?: string }) =>
    typeof v === "number" && !Number.isSafeInteger(v) && c?.source !== undefined && /^-?\d+$/.test(c.source) ? BigInt(c.source) : v;
  return JSON.parse(src, reviver);
}

function req(d: Obj, key: string, where: string): string {
  const v = d[key];
  if (v === undefined || v === null || v === "") throw new ManifestError(`manifest: ${where}${key} is required`);
  return text(v);
}

function obj(d: Obj, key: string, where: string): Obj | null {
  const v = d[key];
  if (v === undefined || v === null) return null;
  if (!isObj(v)) throw new ManifestError(`manifest: ${where}${key} must be an object or null`);
  return v;
}

function list(v: unknown): Obj[] {
  return Array.isArray(v) ? (v as Obj[]) : [];
}

function strs(v: unknown): string[] {
  return Array.isArray(v) ? v.map(text) : [];
}

function str(v: unknown, dflt = ""): string {
  return v === undefined || v === null ? dflt : text(v);
}

/**
 * The manifest types a default as its key's type (25, true); config reads it
 * as the text an env variable would hold, through the same parser.
 */
function defaultText(v: unknown): string {
  if (typeof v === "boolean") return v ? "true" : "false";
  if (v === undefined || v === null) return "";
  return text(v);
}

/** A JSON integer (a number or a bigint parseJSON kept exact) as a bigint. */
function int(v: unknown): bigint {
  if (typeof v === "bigint") return v;
  if (typeof v === "number" && Number.isInteger(v)) return BigInt(v);
  if (v === undefined || v === null) return 0n;
  throw new ManifestError(`manifest: ${json(v)} is not an integer`);
}

/** A JSON bool; absent (or null) is false. "false" is not a bool. */
function bool(d: Obj, key: string, where: string): boolean {
  const v = d[key];
  if (v === undefined || v === null) return false;
  if (typeof v !== "boolean") throw new ManifestError(`manifest: ${where}${key} must be true or false, not ${json(v)}`);
  return v;
}

/** Refuses a value listed twice. */
function unique(values: readonly string[], what: string): void {
  const seen = new Set<string>();
  for (const v of values) {
    if (seen.has(v)) throw new ManifestError(`manifest: ${what} ${v} is listed twice`);
    seen.add(v);
  }
}

// What the Go companion refuses in this section (bizcompanion.Companion.check)
// is refused here too: the companion and the tier must agree on which
// manifests boot.
function companionOf(d: Obj): Companion | null {
  const c = obj(d, "companion", "");
  if (c === null) return null;
  const addressEnv = req(c, "address_env", "companion.");
  const egress: EgressClient[] = list(c.egress).map((e) => ({
    client: req(e, "client", "companion.egress[]."),
    service: req(e, "service", "companion.egress[]."),
    dsnEnv: req(e, "dsn_env", "companion.egress[]."),
  }));
  unique(egress.map((e) => e.client), "companion.egress: client");
  unique(egress.map((e) => e.service), "companion.egress: service");
  const f = obj(c, "files", "companion.");
  let files: FileStorage | null = null;
  if (f !== null) {
    const connections: FileConnection[] = list(f.connections).map((x) => ({
      name: req(x, "name", "companion.files.connections[]."),
      rootEnv: req(x, "root_env", "companion.files.connections[]."),
      defaultRoot: str(x.default_root),
      writable: bool(x, "writable", "companion.files.connections[]."),
    }));
    const names = connections.map((x) => x.name);
    if (new Set(names).size !== names.length) {
      throw new ManifestError(`manifest: companion.files.connections names a connection twice (${names.join(", ")})`);
    }
    const placements: FilePlacement[] = list(f.placements).map((x) => ({
      field: req(x, "field", "companion.files.placements[]."),
      name: str(x.name),
      connection: req(x, "connection", "companion.files.placements[]."),
      bucketPath: str(x.bucket_path),
      expectedObjects: int(x.expected_objects),
      maxPerBucket: Number(int(x.max_per_bucket)),
      maxSizeBytes: int(x.max_size_bytes),
    }));
    unique(placements.map((x) => x.field), "companion.files.placements:");
    for (const x of placements) {
      if (!names.includes(x.connection)) {
        throw new ManifestError(
          `manifest: companion.files.placements: ${x.field} is on connection ${JSON.stringify(x.connection)}, ` +
            "which is not in companion.files.connections",
        );
      }
    }
    files = { service: req(f, "service", "companion.files."), connections, placements };
  }
  return { addressEnv, defaultAddress: str(c.default_address), egress, files };
}

export function fromObject(d: Obj, baseDir = "."): Manifest {
  const fmt = d.format;
  const formats = SUPPORTED_FORMATS.join(" and ");
  if (typeof fmt !== "number" || !Number.isInteger(fmt) || fmt < 1) {
    throw new ManifestError(
      `manifest: format ${json(fmt ?? null)} is not a manifest format version (this SDK reads ${formats})`,
    );
  }
  if (!SUPPORTED_FORMATS.includes(fmt)) {
    throw new ManifestError(
      `manifest: format ${fmt} is not one this SDK reads (it reads formats ${formats}) — ` +
        "install the @w17/business release of the project's sdk_version",
    );
  }
  // An absent language is Python's default, as the Python SDK reads it.
  const language = str(d.language, "python");
  if (language !== LANGUAGE) {
    throw new ManifestError(
      `manifest: language ${JSON.stringify(language)} is not typescript — this is the TypeScript business SDK`,
    );
  }
  const domain = req(d, "domain", "");
  const prefix = str(d.env_prefix) || domain.toUpperCase().replaceAll("-", "_");
  const listen = obj(d, "listen", "") ?? {};
  const reg = obj(d, "register", "");
  if (reg === null) throw new ManifestError("manifest: register is required");
  const path = req(reg, "path", "register.");
  const pkg = str(reg.package) || basename(path);
  const register: Register = {
    path,
    package: pkg,
    module: str(reg.module) || "w17.ts",
    function: str(reg.function) || "registerBusiness",
  };
  const services: ServiceSpec[] = list(d.services).map((s) => {
    const name = req(s, "name", "services[].");
    return {
      name,
      methods: list(s.methods).map((m) => {
        const mname = req(m, "name", "services[].methods[].");
        return {
          name: mname,
          path: str(m.path) || `/${name}/${mname}`,
          input: str(m.input),
          output: str(m.output),
          clientStreaming: Boolean(m.client_streaming),
          serverStreaming: Boolean(m.server_streaming),
        };
      }),
    };
  });
  const config: ConfigKey[] = list(d.config).map((c) => {
    const name = req(c, "name", "config[].");
    return {
      name,
      env: str(c.env) || `${prefix}_${name.toUpperCase()}`,
      type: str(c.type) || "string",
      default: defaultText(c.default),
      required: Boolean(c.required),
      secret: Boolean(c.secret),
      description: str(c.description),
    };
  });
  const st = obj(d, "storage", "");
  let storage: StorageTier | null = null;
  if (st !== null) {
    const dt = st.distributed_transactions;
    storage = {
      addressEnv: req(st, "address_env", "storage."),
      defaultAddress: str(st.default_address),
      services: strs(st.services),
      distributedTransactions: isObj(dt) ? str(dt.service) : "",
    };
  }
  const ev = obj(d, "events", "");
  let events: Events | null = null;
  if (ev !== null) {
    const envelope = str(ev.envelope);
    const nats = isObj(ev.nats) ? ev.nats : {};
    const redis = isObj(ev.redis_streams) ? ev.redis_streams : {};
    const emits: Emit[] = list(ev.emits).map((e) => ({
      method: req(e, "method", "events.emits[]."),
      event: req(e, "event", "events.emits[]."),
      envelope: str(e.envelope) || envelope,
      envelopeField: req(e, "envelope_field", "events.emits[]."),
      channel: req(e, "channel", "events.emits[]."),
      topic: req(e, "topic", "events.emits[]."),
      fields: list(e.fields).map((f) => ({
        field: req(f, "field", "events.emits[].fields[]."),
        source: str(f.source) || "literal",
        path: strs(f.path),
        value: str(f.value),
      })),
    }));
    for (const e of emits) {
      if (!e.envelope) throw new ManifestError(`manifest: events.envelope is required for the emit of ${e.method}`);
    }
    const invalidates: Invalidate[] = list(ev.invalidates).map((i) => ({
      method: req(i, "method", "events.invalidates[]."),
      channel: req(i, "channel", "events.invalidates[]."),
      topic: str(i.topic) || "w17.invalidate",
      group: str(i.group),
      entities: strs(i.entities),
    }));
    events = {
      natsDsnEnv: str(nats.dsn_env),
      redisDsnEnv: str(redis.dsn_env),
      redisChannels: strs(redis.channels),
      durablePrefixEnv: str(ev.durable_prefix_env),
      durablePrefixDefault: str(ev.durable_prefix_default),
      envelope,
      emits,
      invalidates,
    };
  }
  const companion = companionOf(d);
  if (companion !== null && fmt < 2) {
    throw new ManifestError(`manifest: format ${fmt} has no companion section; a manifest with one is format 2`);
  }
  const pluginHooks: PluginHook[] = list(d.plugin_hooks).map((h) => ({
    plugin: str(h.plugin),
    registeredAs: str(h.registered_as),
    slot: str(h.slot),
    methods: strs(h.methods),
  }));
  const extra: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(d)) if (!TOP.has(k)) extra[k] = v;
  const timeout = Number(d.shutdown_timeout_seconds) || 10;
  return {
    format: fmt,
    domain,
    bundle: str(d.bundle) || `${domain}-business`,
    language,
    sdkVersion: str(d.sdk_version),
    envPrefix: prefix,
    listenEnv: str(listen.env) || `${prefix}_LISTEN_ADDR`,
    defaultListenAddr: str(listen.default) || ":50051",
    reflectionEnv: str(d.reflection_env) || `${prefix}_GRPC_REFLECTION`,
    shutdownTimeoutMs: timeout * 1000,
    descriptorSet: str(d.descriptor_set) || "descriptors.binpb",
    register,
    services,
    config,
    storage,
    events,
    pluginHooks,
    companion,
    baseDir,
    extra,
  };
}

export function load(path: string): Manifest {
  let text: string;
  try {
    text = readFileSync(path, "utf8");
  } catch (e) {
    throw new ManifestError(`manifest: read ${path}: ${(e as Error).message}`);
  }
  let d: unknown;
  try {
    d = parseJSON(text);
  } catch (e) {
    throw new ManifestError(`manifest: ${path} is not JSON: ${(e as Error).message}`);
  }
  if (!isObj(d)) throw new ManifestError(`manifest: ${path} is not a JSON object`);
  return fromObject(d, dirname(path));
}
