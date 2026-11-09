/**
 * The tier's configuration: every key the manifest declares, read at boot.
 *
 * Go generates a typed `EnvConfig` struct; the dynamic SDKs get the same
 * values behind typed getters instead. The rules are Go's `envStr` /
 * `envBool` / `envInt`:
 *
 * - an unset (or empty) variable takes the declared default;
 * - a value that does not parse as the declared type stops the boot, naming
 *   the variable — a typo in a production env must not run with a default;
 * - `bool` accepts `true`, `false`, `1`, `0` and nothing else;
 * - `int` is a base-10 64-bit integer (Go's `strconv.Atoi`);
 * - a `secret` key is resolved through the secrets chain (env, `.secrets.age`,
 *   `.secrets`) and comes back as a `Secret` that never prints its value.
 *
 * Beyond Go: a key declared `required` with no value and no default stops the
 * boot too, naming the key and its variable.
 *
 * JavaScript numbers are exact up to 2^53: `cfg.int()` returns a number and
 * refuses (naming the key) a value beyond that; `cfg.bigint()` returns any
 * 64-bit value exactly.
 */

import type { ConfigKey } from "./manifest.js";
import { type Env, type Resolver, Secret, newResolver } from "./secrets.js";

const INT = /^[+-]?[0-9]+$/;
const INT64_MIN = -(2n ** 63n);
const INT64_MAX = 2n ** 63n - 1n;

export class ConfigError extends Error {
  override name = "ConfigError";
}

type Value = string | boolean | bigint | Secret;

function parse(k: ConfigKey, raw: string): Value {
  if (k.type === "" || k.type === "string") return raw;
  if (k.type === "bool") {
    if (raw === "true" || raw === "1") return true;
    if (raw === "false" || raw === "0") return false;
    throw new ConfigError(`env ${k.env}: invalid bool ${JSON.stringify(raw)} (want true, false, 1 or 0)`);
  }
  if (k.type === "int") {
    if (!INT.test(raw)) throw new ConfigError(`env ${k.env}: invalid int ${JSON.stringify(raw)}`);
    // Go's strconv.Atoi on a 64-bit platform: a value past int64 is refused.
    const digits = raw.replace(/^[+-]/, "").replace(/^0+/, "");
    const n = digits.length > 19 ? 0n : BigInt(raw.startsWith("+") ? raw.slice(1) : raw);
    if (digits.length > 19 || n < INT64_MIN || n > INT64_MAX) {
      throw new ConfigError(`env ${k.env}: invalid int ${JSON.stringify(raw)}: value out of range (64-bit)`);
    }
    return n;
  }
  throw new ConfigError(`config key ${JSON.stringify(k.name)}: unknown type ${JSON.stringify(k.type)} (string, bool, int)`);
}

/**
 * The declared configuration, by key name.
 *
 * `cfg.str("greeting")`, `cfg.bool("verbose")`, `cfg.int("limit")`,
 * `cfg.secret("api_token").reveal()`; `cfg.get("greeting")` returns the typed
 * value. Reading an undeclared key throws — a typo must not read as "unset".
 */
export class Config {
  readonly #keys: ReadonlyMap<string, ConfigKey>;
  readonly #values: ReadonlyMap<string, Value>;

  constructor(keys: readonly ConfigKey[], values: ReadonlyMap<string, Value>) {
    this.#keys = new Map(keys.map((k) => [k.name, k]));
    this.#values = values;
  }

  static async load(keys: readonly ConfigKey[], environ: Env = process.env, resolver?: Resolver): Promise<Config> {
    const env = environ;
    const values = new Map<string, Value>();
    if (keys.some((k) => k.secret) && resolver === undefined) {
      try {
        resolver = await newResolver(env);
      } catch (e) {
        throw new ConfigError(`secrets: ${(e as Error).message}`);
      }
    }
    for (const k of keys) {
      if (k.secret) {
        const raw = resolver ? resolver.get(k.env) : "";
        if (!raw && k.required) throw new ConfigError(`config key '${k.name}': ${k.env} is required`);
        values.set(k.name, new Secret(raw || undefined));
        continue;
      }
      let raw = env[k.env] ?? "";
      if (raw === "") raw = k.default;
      if (raw === "") {
        if (k.required) throw new ConfigError(`config key '${k.name}': ${k.env} is required`);
        values.set(k.name, k.type === "bool" ? false : k.type === "int" ? 0n : "");
        continue;
      }
      values.set(k.name, parse(k, raw));
    }
    return new Config(keys, values);
  }

  #get(name: string, kind: string): Value {
    const k = this.#keys.get(name);
    if (k === undefined) {
      throw new RangeError(`config: no key ${JSON.stringify(name)} is declared (declared: ${[...this.#keys.keys()].sort().join(", ")})`);
    }
    const want = k.secret ? "secret" : k.type || "string";
    if (want !== kind) throw new TypeError(`config: key ${JSON.stringify(name)} is a ${want}, not a ${kind}`);
    return this.#values.get(name)!;
  }

  str(name: string): string {
    return this.#get(name, "string") as string;
  }

  bool(name: string): boolean {
    return this.#get(name, "bool") as boolean;
  }

  /** A number; throws for a value beyond JavaScript's exact integers (2^53). */
  int(name: string): number {
    const v = this.#get(name, "int") as bigint;
    const n = Number(v);
    if (!Number.isSafeInteger(n)) {
      throw new RangeError(`config: key ${JSON.stringify(name)} = ${v} is beyond 2^53; read it with bigint()`);
    }
    return n;
  }

  /** The exact 64-bit value of an int key. */
  bigint(name: string): bigint {
    return this.#get(name, "int") as bigint;
  }

  secret(name: string): Secret {
    return this.#get(name, "secret") as Secret;
  }

  /** The typed value (a bigint for an int key). */
  get(name: string): Value {
    if (!this.#keys.has(name)) throw new RangeError(`config: no key ${JSON.stringify(name)} is declared`);
    return this.#values.get(name)!;
  }

  has(name: string): boolean {
    return this.#keys.has(name);
  }

  keys(): string[] {
    return [...this.#keys.keys()];
  }

  secretEnvNames(): string[] {
    return [...this.#keys.values()].filter((k) => k.secret).map((k) => k.env);
  }

  toString(): string {
    const parts = [...this.#values].map(([k, v]) => `${k}: ${v instanceof Secret ? "***" : String(v)}`);
    return `Config({${parts.join(", ")}})`;
  }

  toJSON(): Record<string, unknown> {
    const o: Record<string, unknown> = {};
    for (const [k, v] of this.#values) o[k] = v instanceof Secret ? "***" : typeof v === "bigint" ? v.toString() : v;
    return o;
  }
}
