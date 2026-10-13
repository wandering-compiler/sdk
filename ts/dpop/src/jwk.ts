// JWK thumbprints (RFC 7638) and the public device key.

import { base64urlEncode, utf8 } from "./base64url";

/** The public half of a P-256 device key, exactly the members a proof carries. */
export interface PublicJwk {
  kty: "EC";
  crv: "P-256";
  x: string;
  y: string;
}

/**
 * The required members of each key type, in the lexicographic order RFC 7638
 * §3.2 serialises them in. RSA is here so the RFC's own example (§3.1) can be
 * checked; a device key is always EC P-256, and OKP keeps the thumbprint
 * computable for an Ed25519 key the Go side may hold.
 */
const REQUIRED: Record<string, readonly string[]> = {
  EC: ["crv", "kty", "x", "y"],
  OKP: ["crv", "kty", "x"],
  RSA: ["e", "kty", "n"],
};

/**
 * The RFC 7638 JSON the thumbprint hashes: the required members only, sorted,
 * with no whitespace. Every member value is a string, and JSON.stringify of a
 * string is the RFC 8259 form the RFC asks for (§3.3).
 */
export function canonicalJwk(jwk: Record<string, unknown>): string {
  const kty = jwk.kty;
  if (typeof kty !== "string") throw new Error("dpop: jwk has no kty");
  const members = REQUIRED[kty];
  if (!members) throw new Error(`dpop: jwk kty ${JSON.stringify(kty)} not supported`);
  const parts = members.map((m) => {
    const v = jwk[m];
    if (typeof v !== "string" || v === "") throw new Error(`dpop: jwk member ${m} missing`);
    return `${JSON.stringify(m)}:${JSON.stringify(v)}`;
  });
  return `{${parts.join(",")}}`;
}

/** The RFC 7638 SHA-256 thumbprint of a JWK, base64url without padding: the device id. */
export async function jwkThumbprint(jwk: Record<string, unknown>): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", utf8(canonicalJwk(jwk)));
  return base64urlEncode(new Uint8Array(digest));
}

/**
 * Exports the public key of a P-256 pair as the JWK a proof carries. Only the
 * four public members are copied: WebCrypto's export adds `ext` and
 * `key_ops`, and nothing private may ever reach a header.
 */
export async function publicJwk(publicKey: CryptoKey): Promise<PublicJwk> {
  const alg = publicKey.algorithm as EcKeyAlgorithm;
  if (publicKey.type !== "public" || alg.name !== "ECDSA" || alg.namedCurve !== "P-256") {
    throw new Error("dpop: device key must be an ECDSA P-256 public key");
  }
  const raw = await crypto.subtle.exportKey("jwk", publicKey);
  if (raw.kty !== "EC" || raw.crv !== "P-256" || !raw.x || !raw.y) {
    throw new Error("dpop: exported key is not an EC P-256 JWK");
  }
  return { kty: "EC", crv: "P-256", x: raw.x, y: raw.y };
}
