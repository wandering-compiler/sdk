/**
 * Secret resolution, as sdk/go/service/secret resolves it.
 *
 * The chain, first hit wins:
 *
 * 1. the process environment;
 * 2. `.secrets.age` (`W17_SECRETS_AGE_FILE`), decrypted with the age identity
 *    in `W17_SECRETS_AGE_KEY` or the file `W17_SECRETS_AGE_KEY_FILE`
 *    (fallbacks `SOPS_AGE_KEY` / `SOPS_AGE_KEY_FILE`);
 * 3. `.secrets` (`W17_SECRETS_FILE`), plain dotenv.
 *
 * `W17_SECRETS_MODE`: `auto` (default: the age file when a key AND the file
 * exist, then the plain file when it exists), `plain` (env + plain file only),
 * `age` (strict: a key is required, the age file must decrypt). A corrupt or
 * undecryptable age file, or `age` with no key, fails the boot rather than
 * resolving every secret to empty.
 *
 * The age file is the binary age format filippo.io/age reads (`age.Decrypt`,
 * X25519 identities); `age-encryption` (by the same author) implements it.
 */

import { readFileSync, statSync } from "node:fs";
import { inspect } from "node:util";

export const REDACTED = "***";

export class SecretsError extends Error {
  override name = "SecretsError";
}

/**
 * A secret value that does not print itself.
 *
 * `String(s)`, template literals, `JSON.stringify` and `util.inspect` (so
 * `console.log`) all render `***` (Go's `secret.String`), so a config dumped
 * to a log, Sentry or an error message carries no value. `reveal()` is the
 * one way to read it.
 */
export class Secret {
  readonly #v: string | undefined;

  constructor(value: string | undefined | null) {
    this.#v = value ?? undefined;
  }

  reveal(): string {
    return this.#v ?? "";
  }

  isZero(): boolean {
    return !this.#v;
  }

  equals(other: unknown): boolean {
    return other instanceof Secret && other.#v === this.#v;
  }

  toString(): string {
    return REDACTED;
  }

  toJSON(): string {
    return REDACTED;
  }

  [Symbol.toPrimitive](): string {
    return REDACTED;
  }

  [inspect.custom](): string {
    return REDACTED;
  }
}

/**
 * Go's parseDotenv: `KEY=value` lines, `#` comments and blank lines skipped,
 * the key trimmed, the value verbatim (no quote stripping) less a trailing CR.
 */
export function parseDotenv(s: string): Record<string, string> {
  const kv: Record<string, string> = {};
  for (const line of s.split("\n")) {
    const t = line.trim();
    if (!t || t.startsWith("#")) continue;
    const eq = line.indexOf("=");
    if (eq < 0) continue;
    const key = line.slice(0, eq).trim();
    if (!key) continue;
    let v = line.slice(eq + 1);
    if (v.endsWith("\r")) v = v.slice(0, -1);
    kv[key] = v;
  }
  return kv;
}

export type Env = Readonly<Record<string, string | undefined>>;

export class Resolver {
  readonly #sources: readonly Env[];
  constructor(sources: readonly Env[]) {
    this.#sources = sources;
  }

  lookup(key: string): [string, boolean] {
    for (const s of this.#sources) {
      const v = s[key];
      if (v !== undefined && Object.hasOwn(s, key)) return [v, true];
    }
    return ["", false];
  }

  get(key: string): string {
    return this.lookup(key)[0];
  }
}

async function decryptAge(path: string, keyData: string): Promise<Record<string, string>> {
  const age = await import("age-encryption");
  const d = new age.Decrypter();
  let n = 0;
  for (const raw of keyData.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    try {
      d.addIdentity(line);
    } catch (e) {
      throw new SecretsError(`secret: parse age identity: ${(e as Error).message}`);
    }
    n++;
  }
  if (n === 0) throw new SecretsError("secret: no age identities found in key");
  let data: Uint8Array;
  try {
    data = readFileSync(path);
  } catch (e) {
    throw new SecretsError(`secret: open age file ${path}: ${(e as Error).message}`);
  }
  let plain: string;
  try {
    plain = await d.decrypt(data, "text");
  } catch (e) {
    throw new SecretsError(`secret: decrypt ${path} (wrong key?): ${(e as Error).message}`);
  }
  return parseDotenv(plain);
}

function isFile(p: string): boolean {
  if (!p) return false;
  try {
    return statSync(p).isFile();
  } catch {
    return false;
  }
}

export async function newResolver(environ: Env = process.env): Promise<Resolver> {
  const env = environ;
  const envOr = (k: string, d: string) => env[k] || d;
  const mode = envOr("W17_SECRETS_MODE", "auto").trim().toLowerCase() || "auto";
  const secretsFile = envOr("W17_SECRETS_FILE", ".secrets");
  const ageFile = envOr("W17_SECRETS_AGE_FILE", ".secrets.age");
  const ageKey = env.W17_SECRETS_AGE_KEY || env.SOPS_AGE_KEY || "";
  const ageKeyFile = env.W17_SECRETS_AGE_KEY_FILE || env.SOPS_AGE_KEY_FILE || "";

  let keyData = "";
  let haveKey = false;
  if (ageKey.trim()) {
    keyData = ageKey;
    haveKey = true;
  } else if (ageKeyFile.trim()) {
    try {
      keyData = readFileSync(ageKeyFile, "utf8");
      haveKey = true;
    } catch (e) {
      throw new SecretsError(`secret: read age key file ${ageKeyFile}: ${(e as Error).message}`);
    }
  }

  const sources: Env[] = [env];
  const plain = () => {
    if (isFile(secretsFile)) {
      try {
        sources.push(parseDotenv(readFileSync(secretsFile, "utf8")));
      } catch (e) {
        throw new SecretsError(`secret: read plain secrets file ${JSON.stringify(secretsFile)}: ${(e as Error).message}`);
      }
    }
  };

  if (mode === "plain") {
    plain();
  } else if (mode === "age") {
    if (!haveKey) {
      throw new SecretsError(
        "secret: mode=age but no age key (set W17_SECRETS_AGE_KEY or W17_SECRETS_AGE_KEY_FILE)",
      );
    }
    sources.push(await decryptAge(ageFile, keyData));
  } else if (mode === "auto") {
    if (haveKey && isFile(ageFile)) sources.push(await decryptAge(ageFile, keyData));
    plain();
  } else {
    throw new SecretsError(
      `secret: unknown W17_SECRETS_MODE ${JSON.stringify(env.W17_SECRETS_MODE)} (auto|plain|age)`,
    );
  }
  return new Resolver(sources);
}
