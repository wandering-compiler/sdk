// DPoP proofs (RFC 9449 §4): a compact JWS signed with the device key.

import { base64urlEncode, utf8 } from "./base64url";
import type { DpopKey } from "./key";
import type { PublicJwk } from "./jwk";

/** The `typ` header of every proof. */
export const PROOF_TYPE = "dpop+jwt";

/** The proof header. */
export interface ProofHeader {
  typ: typeof PROOF_TYPE;
  alg: "ES256";
  jwk: PublicJwk;
}

/** The proof claims. `ath` and `nonce` are absent, never empty, when unused. */
export interface ProofClaims {
  jti: string;
  htm: string;
  htu: string;
  iat: number;
  ath?: string;
  nonce?: string;
}

/** What one proof is for. */
export interface ProofParams {
  /** The HTTP method, as sent (e.g. `GET`). */
  htm: string;
  /** The target URI. Its query and fragment are dropped ({@link normalizeHTU}). */
  htu: string;
  /** The access token the request carries; its hash becomes `ath`. Omit on a request without one. */
  accessToken?: string | undefined;
  /** The last nonce the server sent for this origin. */
  nonce?: string | undefined;
  /** The clock, for tests. Default `Date.now`. */
  now?: () => number;
}

/**
 * Puts a target URI in the form proofs carry and the server compares: an
 * absolute http or https URI with lower-case scheme and host, no default
 * port, no user info, an empty path written as `/`, and no query or fragment
 * (RFC 9449 §4.2 and §4.3). The path stays exactly as the URL serialises it,
 * which is what fetch sends, so the server's own view of the path matches.
 */
export function normalizeHTU(raw: string): string {
  // The URL parser forgives what an htu must not be: "https:///p" and
  // "https:host/p" both parse as https://p/ and https://host/p. Insist on the
  // authority being written out.
  if (!/^https?:\/\/[^/?#\\]/i.test(raw)) {
    throw new Error(`dpop: htu ${JSON.stringify(raw)}: want an absolute http or https URI`);
  }
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    throw new Error(`dpop: htu ${JSON.stringify(raw)}: not an absolute URI`);
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") {
    throw new Error(`dpop: htu ${JSON.stringify(raw)}: want an absolute http or https URI`);
  }
  if (u.username !== "" || u.password !== "" || u.hostname === "") {
    throw new Error(`dpop: htu ${JSON.stringify(raw)}: want scheme://host[:port]/path`);
  }
  // URL already lower-cases scheme and host, drops the default port, and
  // writes an empty path as "/".
  return `${u.protocol}//${u.host}${u.pathname}`;
}

/** The `ath` of an access token: base64url (no padding) of its SHA-256. */
export async function accessTokenHash(token: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", utf8(token));
  return base64urlEncode(new Uint8Array(digest));
}

/** A fresh jti: 128 random bits, base64url. */
export function newJTI(): string {
  return base64urlEncode(crypto.getRandomValues(new Uint8Array(16)));
}

/**
 * Signs arbitrary claims with the device key. {@link createProof} is the
 * normal entry point; this one exists for callers (and tests) that set iat or
 * jti themselves.
 */
export async function signClaims(key: DpopKey, claims: ProofClaims): Promise<string> {
  const header: ProofHeader = { typ: PROOF_TYPE, alg: "ES256", jwk: { ...key.jwk } };
  const signingInput = `${base64urlEncode(utf8(JSON.stringify(header)))}.${base64urlEncode(utf8(JSON.stringify(claims)))}`;
  const sig = new Uint8Array(
    await crypto.subtle.sign(
      { name: "ECDSA", hash: "SHA-256" },
      key.keyPair.privateKey,
      utf8(signingInput),
    ),
  );
  // JWS ES256 is the raw r || s, 32 bytes each (RFC 7518 §3.4). WebCrypto
  // returns exactly that for ECDSA; an ASN.1 DER signature (70-72 bytes)
  // would be refused by every verifier, so check rather than assume.
  if (sig.length !== 64)
    throw new Error(`dpop: ECDSA signature is ${sig.length} bytes, want 64 (raw r||s)`);
  return `${signingInput}.${base64urlEncode(sig)}`;
}

/** Builds and signs a proof for one request. Each call draws a fresh jti. */
export async function createProof(key: DpopKey, p: ProofParams): Promise<string> {
  if (!p.htm) throw new Error("dpop: proof needs an HTTP method");
  const claims: ProofClaims = {
    jti: newJTI(),
    htm: p.htm,
    htu: normalizeHTU(p.htu),
    iat: Math.floor((p.now ?? Date.now)() / 1000),
  };
  if (p.accessToken) claims.ath = await accessTokenHash(p.accessToken);
  if (p.nonce) claims.nonce = p.nonce;
  return signClaims(key, claims);
}
