/**
 * Internal TLS, read from the same variables as sdk/go/lib/grpcclient and
 * lib/grpcserver.
 *
 * `W17_INTERNAL_TLS`:
 *
 * - `on` | `true` | `1` | `yes` — servers listen TLS, clients dial TLS;
 * - `terminated` — TLS ends at the platform: servers listen plain, clients
 *   still dial TLS (system roots, no client pair);
 * - `off` | `false` | `0` | `no` | unset — plaintext;
 * - anything else refuses the boot (a typo must not silently downgrade).
 *
 * `W17_INTERNAL_TLS_CERT` / `_KEY` are this service's leaf pair (required for
 * a TLS server, presented as the client pair when dialing);
 * `W17_INTERNAL_TLS_CA` is the roots a client verifies against;
 * `W17_INTERNAL_TLS_CLIENT_CA`, when set on a server, requires and verifies a
 * client certificate (mutual TLS).
 */

import { X509Certificate } from "node:crypto";
import { readFileSync } from "node:fs";
import { createSecureContext } from "node:tls";
import { ChannelCredentials, ServerCredentials, credentials } from "@grpc/grpc-js";
import type { Env } from "./secrets.js";

export const ENV_INTERNAL_TLS = "W17_INTERNAL_TLS";
export const ENV_INTERNAL_TLS_CA = "W17_INTERNAL_TLS_CA";
export const ENV_INTERNAL_TLS_CERT = "W17_INTERNAL_TLS_CERT";
export const ENV_INTERNAL_TLS_KEY = "W17_INTERNAL_TLS_KEY";
export const ENV_INTERNAL_TLS_CLIENT_CA = "W17_INTERNAL_TLS_CLIENT_CA";

const norm = (v: string | undefined) => (v ?? "").trim().toLowerCase();

export function enabled(v: string | undefined): boolean {
  return ["on", "true", "1", "yes"].includes(norm(v));
}

export function terminated(v: string | undefined): boolean {
  return norm(v) === "terminated";
}

function off(v: string | undefined): boolean {
  return ["", "off", "false", "0", "no"].includes(norm(v));
}

/** Throws for a value that is not recognised. */
export function check(v: string | undefined): void {
  if (enabled(v) || terminated(v) || off(v)) return;
  throw new RangeError(
    `${ENV_INTERNAL_TLS}=${JSON.stringify(v)} is not a recognised value; accepted: on|true|1|yes (TLS), ` +
      "terminated (TLS ends at the platform; servers listen plain), off|false|0|no or unset (plaintext)",
  );
}

export function dialTLS(v: string | undefined): boolean {
  return enabled(v) || terminated(v);
}

/**
 * Read a CA bundle: exactly the certificates in path (Go's x509.NewCertPool +
 * AppendCertsFromPEM); a path that cannot be read or holds no certificate
 * stops the boot naming the variable.
 */
function loadCA(path: string, variable: string): Buffer {
  let pem: Buffer;
  try {
    pem = readFileSync(path);
  } catch (e) {
    throw new RangeError(`reading ${variable} at ${JSON.stringify(path)}: ${(e as Error).message}`, { cause: e });
  }
  const blocks = pem.toString("latin1").match(/-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----/g) ?? [];
  let n = 0;
  for (const b of blocks) {
    try {
      new X509Certificate(b);
      n++;
    } catch {
      // not a certificate; Go's AppendCertsFromPEM skips it too
    }
  }
  if (n === 0) throw new RangeError(`${variable} at ${JSON.stringify(path)}: no PEM certs found`);
  return pem;
}

function loadPair(cert: string, key: string): { cert: Buffer; key: Buffer } {
  const where = `(${ENV_INTERNAL_TLS_CERT}=${JSON.stringify(cert)} / ${ENV_INTERNAL_TLS_KEY}=${JSON.stringify(key)})`;
  try {
    const pair = { cert: readFileSync(cert), key: readFileSync(key) };
    createSecureContext(pair); // throws for a pair that does not parse or match
    return pair;
  } catch (e) {
    throw new RangeError(`loading the cert/key pair ${where}: ${(e as Error).message}`, { cause: e });
  }
}

/** What a client dials with: plaintext, or TLS with the trust and pair below. */
export interface ClientTLS {
  readonly tls: boolean;
  /** undefined: the system roots (Go's nil RootCAs). */
  readonly rootCerts?: Buffer;
  readonly cert?: Buffer;
  readonly key?: Buffer;
}

/**
 * The TLS a client dials with.
 *
 * An unrecognised value dials TLS (Go's grpcx.InternalTLSEnabled logs and
 * dials TLS: refusing a call is safer than sending it in the clear).
 *
 * Trust is Go's `RootCAs`: `W17_INTERNAL_TLS_CA` set means that CA and
 * nothing else — never the system roots beside it; unset means the system
 * roots. Under `terminated` the platform's certificate is verified against
 * the system roots and the stack's CA and pair are not used.
 *
 * Throws — the boot stops — for a CA that cannot be read or holds no
 * certificate, a cert/key pair half set, or a pair that does not load. (Go's
 * dialer falls back silently in those cases and fails on the first call
 * instead; a misconfigured mesh is refused at boot here.)
 */
export function clientTLS(env: Env = process.env): ClientTLS {
  const v = env[ENV_INTERNAL_TLS];
  let recognised = true;
  try {
    check(v);
  } catch {
    recognised = false;
  }
  if (recognised && !dialTLS(v)) return { tls: false };
  if (terminated(v)) return { tls: true };
  const ca = env[ENV_INTERNAL_TLS_CA] ?? "";
  const rootCerts = ca ? loadCA(ca, ENV_INTERNAL_TLS_CA) : undefined;
  const cert = env[ENV_INTERNAL_TLS_CERT] ?? "";
  const key = env[ENV_INTERNAL_TLS_KEY] ?? "";
  if (Boolean(cert) !== Boolean(key)) {
    throw new RangeError(
      `${ENV_INTERNAL_TLS_CERT} / ${ENV_INTERNAL_TLS_KEY} are not both set — set both (this service's ` +
        "client pair for mutual TLS) or neither",
    );
  }
  const pair = cert ? loadPair(cert, key) : undefined;
  return { tls: true, ...(rootCerts && { rootCerts }), ...(pair && pair) };
}

/** grpc-js channel credentials for a {@link ClientTLS}. */
export function channelCredentials(c: ClientTLS): ChannelCredentials {
  if (!c.tls) return credentials.createInsecure();
  return credentials.createSsl(c.rootCerts ?? null, c.key ?? null, c.cert ?? null);
}

/** What the server listens with. */
export interface ServerTLS {
  readonly tls: boolean;
  readonly cert?: Buffer;
  readonly key?: Buffer;
  /** Set: client certificates are required and verified against it alone. */
  readonly clientCA?: Buffer;
}

/**
 * The TLS the server listens with.
 *
 * Throws for a value that is not recognised, TLS on without a loadable leaf
 * pair, or a `W17_INTERNAL_TLS_CLIENT_CA` that cannot be read — the boot fails
 * naming the variable. Client certificates are verified against that CA alone
 * (Go's `ClientCAs`), never the system's.
 */
export function serverTLS(env: Env = process.env): ServerTLS {
  const v = env[ENV_INTERNAL_TLS];
  check(v);
  if (!enabled(v)) return { tls: false };
  const cert = env[ENV_INTERNAL_TLS_CERT] ?? "";
  const key = env[ENV_INTERNAL_TLS_KEY] ?? "";
  if (!cert || !key) {
    throw new RangeError(
      `${ENV_INTERNAL_TLS}=on but ${ENV_INTERNAL_TLS_CERT} / ${ENV_INTERNAL_TLS_KEY} are not both set — ` +
        "point them at this service's leaf cert+key (dev PKI: w17/certs/dev.local/)",
    );
  }
  const pair = loadPair(cert, key);
  const clientCAPath = env[ENV_INTERNAL_TLS_CLIENT_CA] ?? "";
  const clientCA = clientCAPath ? loadCA(clientCAPath, ENV_INTERNAL_TLS_CLIENT_CA) : undefined;
  return { tls: true, ...pair, ...(clientCA && { clientCA }) };
}

/** grpc-js server credentials for a {@link ServerTLS}. */
export function serverCredentials(s: ServerTLS): ServerCredentials {
  if (!s.tls) return ServerCredentials.createInsecure();
  return ServerCredentials.createSsl(
    s.clientCA ?? null,
    [{ private_key: s.key!, cert_chain: s.cert! }],
    s.clientCA !== undefined,
  );
}

/**
 * The first dNSName of a PEM certificate's subjectAltName, undefined when it
 * has none or cannot be read. The health probe verifies the listener against
 * it, as Go's does: the leaf names this service, not 127.0.0.1.
 */
export function leafServerName(certPath: string): string | undefined {
  try {
    const san = new X509Certificate(readFileSync(certPath)).subjectAltName ?? "";
    for (const part of san.split(",")) {
      const p = part.trim();
      if (p.startsWith("DNS:")) return p.slice(4);
    }
  } catch {
    // no name: the probe verifies against the host it dials
  }
  return undefined;
}

/** Credentials for the OTLP exporter: the mesh's CA and client pair (system roots without a CA). */
export function otlpCredentials(env: Env): ChannelCredentials {
  if (terminated(env[ENV_INTERNAL_TLS])) return credentials.createSsl();
  const read = (name: string) => {
    const p = env[name] ?? "";
    try {
      return p ? readFileSync(p) : null;
    } catch {
      return null;
    }
  };
  return credentials.createSsl(read(ENV_INTERNAL_TLS_CA), read(ENV_INTERNAL_TLS_KEY), read(ENV_INTERNAL_TLS_CERT));
}
