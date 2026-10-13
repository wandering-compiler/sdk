import { describe, expect, it } from "vitest";

import { canonicalJwk, jwkThumbprint, publicJwk } from "../src/jwk";
import { dpopKey, generateKeyPair } from "../src/key";

// RFC 7638 §3.1: the example RSA key and its SHA-256 thumbprint.
const rfc7638 = {
  kty: "RSA",
  n:
    "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAt" +
    "VT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn6" +
    "4tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FD" +
    "W2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n9" +
    "1CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINH" +
    "aQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw",
  e: "AQAB",
  alg: "RS256",
  kid: "2011-04-29",
};

// RFC 9449 §6.1: the P-256 key of the RFC's examples and its jkt — the EC
// vector, the shape every device key has. go/lib/dpop checks the same value
// (TestThumbprint_RFC9449KeyMatchesJKT).
const rfc9449 = {
  kty: "EC",
  crv: "P-256",
  x: "l8tFrhx-34tV3hRICRDY9zCkDlpBhF42UQUfWVAWBFs",
  y: "9VE4jf_Ok_o64zbTTlcuNJajHmt6v9TDVrU0CdvGRDA",
};
const rfc9449JKT = "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I";

describe("jwkThumbprint (RFC 7638)", () => {
  it("matches the RFC 7638 RSA example", async () => {
    expect(await jwkThumbprint(rfc7638)).toBe("NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs");
  });

  it("matches the RFC 9449 EC example's jkt", async () => {
    expect(await jwkThumbprint(rfc9449)).toBe(rfc9449JKT);
  });

  it("hashes only the required members, sorted, without whitespace", () => {
    expect(
      canonicalJwk({ y: "Y", x: "X", kty: "EC", crv: "P-256", d: "secret", kid: "k", use: "sig" }),
    ).toBe('{"crv":"P-256","kty":"EC","x":"X","y":"Y"}');
    expect(canonicalJwk({ kty: "OKP", crv: "Ed25519", x: "X" })).toBe(
      '{"crv":"Ed25519","kty":"OKP","x":"X"}',
    );
  });

  it("is independent of member order and extra members", async () => {
    const shuffled = {
      y: rfc9449.y,
      extra: "1",
      x: rfc9449.x,
      crv: "P-256",
      kty: "EC",
      key_ops: ["verify"],
    };
    expect(await jwkThumbprint(shuffled)).toBe(rfc9449JKT);
  });

  it("refuses a key with a missing member or an unknown kty", () => {
    expect(() => canonicalJwk({ kty: "EC", crv: "P-256", x: "X" })).toThrow(/member y/);
    expect(() => canonicalJwk({ kty: "oct", k: "K" })).toThrow(/not supported/);
    expect(() => canonicalJwk({ crv: "P-256" })).toThrow(/no kty/);
  });
});

describe("publicJwk / dpopKey", () => {
  it("exports exactly the four public members", async () => {
    const pair = await generateKeyPair();
    const jwk = await publicJwk(pair.publicKey);
    expect(Object.keys(jwk).sort()).toEqual(["crv", "kty", "x", "y"]);
    expect(jwk.kty).toBe("EC");
    expect(jwk.crv).toBe("P-256");
    // 32-byte coordinates.
    expect(jwk.x).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(jwk.y).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });

  it("generates a non-extractable private key by default", async () => {
    const pair = await generateKeyPair();
    expect(pair.privateKey.extractable).toBe(false);
    await expect(crypto.subtle.exportKey("pkcs8", pair.privateKey)).rejects.toThrow();
    await expect(crypto.subtle.exportKey("jwk", pair.privateKey)).rejects.toThrow();
  });

  it("computes the thumbprint of the public JWK", async () => {
    const key = await dpopKey(await generateKeyPair());
    expect(key.thumbprint).toBe(await jwkThumbprint({ ...key.jwk }));
  });

  it("refuses a key that is not ECDSA P-256", async () => {
    const p384 = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-384" }, false, [
      "sign",
      "verify",
    ]);
    await expect(dpopKey(p384)).rejects.toThrow(/P-256/);
    await expect(publicJwk(p384.publicKey)).rejects.toThrow(/P-256/);
    const ecdh = await crypto.subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, false, [
      "deriveBits",
    ]);
    await expect(dpopKey(ecdh)).rejects.toThrow(/P-256/);
    const pair = await generateKeyPair();
    await expect(publicJwk(pair.privateKey)).rejects.toThrow(/public/);
  });
});
