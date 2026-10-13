// @w17/dpop/node — a device key in a file, for servers, CLIs and tests.
//
// Kept out of the main entry point so a browser bundle never imports a Node
// module. A browser keeps its key in IndexedDB, non-extractable; a file can
// only hold a key that WebCrypto lets out, so this store generates an
// EXTRACTABLE private key and writes it in the clear (mode 0600). That is a
// deliberate trade for a process with no keystore of its own, and the
// constructor makes the caller say so.

import { randomBytes } from "node:crypto";
import { open, readFile, rename, unlink } from "node:fs/promises";
import { basename, dirname, join } from "node:path";

import { base64urlDecode, base64urlEncode } from "./base64url";
import type { KeyStore } from "./key";

/** Options of {@link FileKeyStore}. */
export interface FileKeyStoreOptions {
  /**
   * Must be `true`: the store writes the private key to disk, so it generates
   * the key extractable. There is no default; leaving it out throws.
   */
  exportPrivateKey: true;
}

const P256: EcKeyImportParams = { name: "ECDSA", namedCurve: "P-256" };
const PEM_LABEL = "PRIVATE KEY";

function toPEM(der: Uint8Array): string {
  const b64 = base64urlEncode(der).replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
  const lines = padded.match(/.{1,64}/g) ?? [];
  return `-----BEGIN ${PEM_LABEL}-----\n${lines.join("\n")}\n-----END ${PEM_LABEL}-----\n`;
}

function fromPEM(pem: string): Uint8Array<ArrayBuffer> {
  const m = /-----BEGIN PRIVATE KEY-----([\s\S]+?)-----END PRIVATE KEY-----/.exec(pem);
  if (!m?.[1]) throw new Error("dpop: no PKCS #8 PRIVATE KEY PEM block");
  const b64 = m[1].replace(/\s+/g, "").replace(/=+$/, "").replace(/\+/g, "-").replace(/\//g, "_");
  return new Uint8Array(base64urlDecode(b64));
}

function isMissing(err: unknown): boolean {
  return (err as { code?: unknown } | null)?.code === "ENOENT";
}

/**
 * A {@link KeyStore} in one file: the private key as a PKCS #8
 * `PRIVATE KEY` PEM block with mode 0600 — the format the Go SDK's
 * `dpop.Key.SaveFile` writes and `dpop.LoadKeyFile` reads, so a Go and a
 * TypeScript process can share one device key. Writes go to a temporary file
 * next to the target and are renamed over it.
 */
export class FileKeyStore implements KeyStore {
  readonly needsExtractableKey = true;
  readonly path: string;

  constructor(path: string, opts: FileKeyStoreOptions) {
    if (opts?.exportPrivateKey !== true) {
      throw new Error(
        "dpop: FileKeyStore writes the private key to disk; pass { exportPrivateKey: true } to accept that",
      );
    }
    if (!path) throw new Error("dpop: FileKeyStore needs a path");
    this.path = path;
  }

  async load(): Promise<CryptoKeyPair | undefined> {
    let pem: string;
    try {
      pem = await readFile(this.path, "utf8");
    } catch (err) {
      if (isMissing(err)) return undefined;
      throw err;
    }
    const privateKey = await crypto.subtle.importKey("pkcs8", fromPEM(pem), P256, true, ["sign"]);
    // WebCrypto cannot derive a public key from a private one directly; the
    // private JWK carries x and y, and only those go into the public key.
    const jwk = await crypto.subtle.exportKey("jwk", privateKey);
    if (!jwk.x || !jwk.y) throw new Error("dpop: key file is not an EC P-256 key");
    const publicKey = await crypto.subtle.importKey(
      "jwk",
      { kty: "EC", crv: "P-256", x: jwk.x, y: jwk.y },
      P256,
      true,
      ["verify"],
    );
    return { privateKey, publicKey };
  }

  async save(keyPair: CryptoKeyPair): Promise<void> {
    const der = new Uint8Array(await crypto.subtle.exportKey("pkcs8", keyPair.privateKey));
    const tmp = join(
      dirname(this.path),
      `.${basename(this.path)}.tmp${randomBytes(6).toString("hex")}`,
    );
    const fh = await open(tmp, "wx", 0o600);
    let renamed = false;
    try {
      await fh.writeFile(toPEM(der));
      await fh.sync();
      await fh.close();
      await rename(tmp, this.path);
      renamed = true;
    } finally {
      if (!renamed) {
        await fh.close().catch(() => undefined);
        await unlink(tmp).catch(() => undefined);
      }
    }
  }

  async clear(): Promise<void> {
    try {
      await unlink(this.path);
    } catch (err) {
      if (!isMissing(err)) throw err;
    }
  }
}
