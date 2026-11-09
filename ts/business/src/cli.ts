/**
 * `w17-business serve|health [--manifest PATH]` (`node dist/main.js ...`).
 *
 * - `serve` (also the default with no command) boots and serves the tier —
 *   what the container image runs.
 * - `health [--timeout DURATION]` asks the listener this tier opens whether
 *   it answers `grpc.health.v1` SERVING, on this host — the container
 *   HEALTHCHECK, as Go's `<binary> health`. Exit 0 when it answered, 1
 *   naming it otherwise. The address comes from the same environment the
 *   server reads.
 */

import { ManifestError, load } from "./manifest.js";
import { BootError, resolveManifestPath, serve } from "./runtime.js";
import * as health from "./health.js";
import type { Env } from "./secrets.js";
import * as tlsconfig from "./tlsconfig.js";

export const DEFAULT_HEALTH_TIMEOUT_MS = 3000;

const DUR = /^(\d+(?:\.\d+)?)(ms|s|m)$/;

/** Go's duration syntax for the cases a probe takes: `500ms`, `3s`, `1m`; milliseconds. */
export function parseDuration(s: string): number {
  const m = DUR.exec(s.trim());
  if (!m) throw new RangeError(`invalid duration ${JSON.stringify(s)}`);
  const n = Number(m[1]);
  return m[2] === "ms" ? n : m[2] === "m" ? n * 60_000 : n * 1000;
}

/** The address the probe dials for a listen address: a wildcard host is this host. */
export function probeTarget(listen: string): [string, number] {
  const i = listen.lastIndexOf(":");
  const port = i < 0 ? "" : listen.slice(i + 1);
  if (i < 0 || !/^[0-9]+$/.test(port) || Number(port) === 0) {
    throw new RangeError(`listen address ${JSON.stringify(listen)} has no fixed port to probe`);
  }
  let host = listen.slice(0, i).replace(/^\[|\]$/g, "");
  if (host === "" || host === "0.0.0.0" || host === "::") host = "127.0.0.1";
  return [host, Number(port)];
}

/** One health check against listen; throws naming what failed. */
export async function probe(listen: string, env: Env, timeoutMs: number): Promise<void> {
  const [host, port] = probeTarget(listen);
  const v = env[tlsconfig.ENV_INTERNAL_TLS];
  tlsconfig.check(v);
  let c: tlsconfig.ClientTLS = { tls: false };
  const options: Record<string, string> = {};
  if (tlsconfig.enabled(v)) {
    const cert = env[tlsconfig.ENV_INTERNAL_TLS_CERT] ?? "";
    const key = env[tlsconfig.ENV_INTERNAL_TLS_KEY] ?? "";
    if (!cert || !key) {
      throw new RangeError(
        `${tlsconfig.ENV_INTERNAL_TLS} is on but ${tlsconfig.ENV_INTERNAL_TLS_CERT} / ` +
          `${tlsconfig.ENV_INTERNAL_TLS_KEY} are not both set — the listener cannot be serving TLS either`,
      );
    }
    c = tlsconfig.clientTLS({ ...env, [tlsconfig.ENV_INTERNAL_TLS]: "on" });
    // The listener's certificate names this service, not 127.0.0.1: verify
    // the probe against the container's own leaf's first DNS name, as Go does.
    const name = tlsconfig.leafServerName(cert);
    if (name) {
      options["grpc.ssl_target_name_override"] = name;
      options["grpc.default_authority"] = name;
    }
  }
  const status = await health.check(`${host}:${port}`, tlsconfig.channelCredentials(c), timeoutMs, options);
  if (status !== health.ServingStatus.SERVING) throw new Error(`grpc.health.v1 status ${health.servingStatusName(status)}`);
}

export async function runHealth(manifest: string | undefined, timeoutMs: number, env: Env = process.env): Promise<number> {
  const m = load(resolveManifestPath(manifest, env));
  const listen = env[m.listenEnv] || m.defaultListenAddr;
  try {
    await probe(listen, env, timeoutMs);
  } catch (e) {
    console.log(`FAIL grpc ${listen}: ${e instanceof Error ? e.message : String(e)}`);
    console.error("health: grpc not answering");
    return 1;
  }
  console.log(`ok   grpc ${listen}`);
  return 0;
}

const USAGE = `usage: w17-business [serve|health] [options]

  serve     boot and serve the tier (default)
            --manifest PATH       w17-business.json (default $W17_BUSINESS_MANIFEST, else ./w17-business.json)
            --project-root DIR    the directory register.path is relative to
                                  (default $W17_BUSINESS_PROJECT_ROOT, else the working directory)
  health    probe this tier's listener (a container HEALTHCHECK)
            --manifest PATH
            --timeout DURATION    bound on the whole probe (default 3s)
`;

interface Args {
  cmd: string;
  flags: Map<string, string>;
}

function parseArgs(argv: readonly string[]): Args {
  const flags = new Map<string, string>();
  let cmd = "";
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]!;
    if (a === "-h" || a === "--help") {
      flags.set("help", "1");
    } else if (a.startsWith("--")) {
      const eq = a.indexOf("=");
      if (eq > 0) flags.set(a.slice(2, eq), a.slice(eq + 1));
      else {
        const v = argv[i + 1];
        if (v === undefined) throw new RangeError(`${a} needs a value`);
        flags.set(a.slice(2), v);
        i++;
      }
    } else if (!cmd) {
      cmd = a;
    } else {
      throw new RangeError(`unexpected argument ${JSON.stringify(a)}`);
    }
  }
  return { cmd: cmd || "serve", flags };
}

const ALLOWED: Record<string, readonly string[]> = {
  serve: ["manifest", "project-root"],
  health: ["manifest", "timeout"],
};

export async function main(argv: readonly string[]): Promise<number> {
  let args: Args;
  try {
    args = parseArgs(argv);
  } catch (e) {
    console.error(`w17-business: ${(e as Error).message}\n${USAGE}`);
    return 2;
  }
  if (args.flags.has("help")) {
    console.log(USAGE);
    return 0;
  }
  const allowed = ALLOWED[args.cmd];
  if (allowed === undefined) {
    console.error(`w17-business: unknown command ${JSON.stringify(args.cmd)}\n${USAGE}`);
    return 2;
  }
  for (const k of args.flags.keys()) {
    if (!allowed.includes(k)) {
      console.error(`w17-business ${args.cmd}: unknown flag --${k}\n${USAGE}`);
      return 2;
    }
  }
  try {
    if (args.cmd === "health") {
      const raw = args.flags.get("timeout") ?? "3s";
      let t: number;
      try {
        t = parseDuration(raw);
      } catch (e) {
        console.error(`health: ${(e as Error).message}`);
        return 2;
      }
      if (t <= 0) {
        console.error(`health: --timeout must be positive, got ${raw}`);
        return 2;
      }
      return await runHealth(args.flags.get("manifest"), t);
    }
    await serve(args.flags.get("manifest"), args.flags.get("project-root"));
    return 0;
  } catch (e) {
    if (e instanceof BootError || e instanceof ManifestError) {
      console.error(`w17-business: ${e.message}`);
      return 1;
    }
    throw e;
  }
}
