import { describe, expect, it } from "vitest";

import { base64urlEncode, utf8 } from "../src/base64url";
import { dpopKey, generateKeyPair, type DpopKey } from "../src/key";
import { accessTokenHash, createProof, newJTI, normalizeHTU, signClaims } from "../src/proof";
import { decodeProof, verifyProof } from "./helpers";

const key = (): Promise<DpopKey> => generateKeyPair().then(dpopKey);

// RFC 9449 Figure 2 and Figure 13: proofs signed by the RFC's example key. The
// same strings are verified by go/lib/dpop (TestVerify_RFC9449Examples). They
// check the test's own verifier — base64url decoding and WebCrypto accepting
// a raw r||s signature — against signatures this package did not make.
const fig2 =
  "eyJ0eXAiOiJkcG9wK2p3dCIsImFsZyI6IkVTMjU2IiwiandrIjp7Imt0eSI6Ik" +
  "VDIiwieCI6Imw4dEZyaHgtMzR0VjNoUklDUkRZOXpDa0RscEJoRjQyVVFVZldWQVdCR" +
  "nMiLCJ5IjoiOVZFNGpmX09rX282NHpiVFRsY3VOSmFqSG10NnY5VERWclUwQ2R2R1JE" +
  "QSIsImNydiI6IlAtMjU2In19.eyJqdGkiOiItQndDM0VTYzZhY2MybFRjIiwiaHRtIj" +
  "oiUE9TVCIsImh0dSI6Imh0dHBzOi8vc2VydmVyLmV4YW1wbGUuY29tL3Rva2VuIiwia" +
  "WF0IjoxNTYyMjYyNjE2fQ.2-GxA6T8lP4vfrg8v-FdWP0A0zdrj8igiMLvqRMUvwnQg" +
  "4PtFLbdLXiOSsX0x7NVY-FNyJK70nfbV37xRZT3Lg";
const fig13 =
  "eyJ0eXAiOiJkcG9wK2p3dCIsImFsZyI6IkVTMjU2IiwiandrIjp7Imt0eSI6Ik" +
  "VDIiwieCI6Imw4dEZyaHgtMzR0VjNoUklDUkRZOXpDa0RscEJoRjQyVVFVZldWQVdCR" +
  "nMiLCJ5IjoiOVZFNGpmX09rX282NHpiVFRsY3VOSmFqSG10NnY5VERWclUwQ2R2R1JE" +
  "QSIsImNydiI6IlAtMjU2In19.eyJqdGkiOiJlMWozVl9iS2ljOC1MQUVCIiwiaHRtIj" +
  "oiR0VUIiwiaHR1IjoiaHR0cHM6Ly9yZXNvdXJjZS5leGFtcGxlLm9yZy9wcm90ZWN0Z" +
  "WRyZXNvdXJjZSIsImlhdCI6MTU2MjI2MjYxOCwiYXRoIjoiZlVIeU8ycjJaM0RaNTNF" +
  "c05yV0JiMHhXWG9hTnk1OUlpS0NBcWtzbVFFbyJ9.2oW9RP35yRqzhrtNP86L-Ey71E" +
  "OptxRimPPToA1plemAgR6pxHF8y6-yqyVnmcw6Fy1dqd-jfxSYoMxhAJpLjA";
const rfcToken = "Kz~8mXK1EalYznwH-LC-1fBAo.4Ljp~zsPE_NeO.gxU";
const rfcATH = "fUHyO2r2Z3DZ53EsNrWBb0xWXoaNy59IiKCAqksmQEo";

describe("RFC 9449 vectors", () => {
  it("verifies the RFC's example proofs", async () => {
    expect(await verifyProof(fig2)).toBe(true);
    expect(await verifyProof(fig13)).toBe(true);
    expect(decodeProof(fig13).claims.ath).toBe(rfcATH);
  });

  it("refuses an RFC proof with one bit of its signature flipped", async () => {
    const d = decodeProof(fig2);
    d.signature[10] = d.signature[10]! ^ 1;
    expect(await verifyProof(`${d.signingInput}.${base64urlEncode(d.signature)}`)).toBe(false);
  });

  it("computes the RFC's ath", async () => {
    expect(await accessTokenHash(rfcToken)).toBe(rfcATH);
  });
});

describe("createProof", () => {
  it("has the RFC 9449 header: typ, ES256, the public jwk and nothing else", async () => {
    const k = await key();
    const d = decodeProof(await createProof(k, { htm: "GET", htu: "https://api.example.com/r" }));
    expect(d.header).toEqual({ typ: "dpop+jwt", alg: "ES256", jwk: k.jwk });
    expect(Object.keys(d.header.jwk as object).sort()).toEqual(["crv", "kty", "x", "y"]);
  });

  it("carries jti, htm, htu and iat, and no ath or nonce unless given", async () => {
    const k = await key();
    const d = decodeProof(
      await createProof(k, {
        htm: "POST",
        htu: "https://api.example.com/r",
        now: () => 1_800_000_000_999,
      }),
    );
    expect(Object.keys(d.claims).sort()).toEqual(["htm", "htu", "iat", "jti"]);
    expect(d.claims).toMatchObject({
      htm: "POST",
      htu: "https://api.example.com/r",
      iat: 1_800_000_000,
    });
    const empty = decodeProof(
      await createProof(k, { htm: "GET", htu: "https://a.example/", accessToken: "", nonce: "" }),
    );
    expect(empty.claims).not.toHaveProperty("ath");
    expect(empty.claims).not.toHaveProperty("nonce");
  });

  it("sets ath to the token hash and nonce to the server nonce", async () => {
    const k = await key();
    const d = decodeProof(
      await createProof(k, {
        htm: "GET",
        htu: "https://resource.example.org/x",
        accessToken: rfcToken,
        nonce: "n-1",
      }),
    );
    expect(d.claims.ath).toBe(rfcATH);
    expect(d.claims.nonce).toBe("n-1");
  });

  it("draws a fresh 128-bit jti per proof", async () => {
    const k = await key();
    const jtis = new Set<unknown>();
    for (let i = 0; i < 50; i++) {
      jtis.add(
        decodeProof(await createProof(k, { htm: "GET", htu: "https://a.example/" })).claims.jti,
      );
    }
    expect(jtis.size).toBe(50);
    expect(newJTI()).toMatch(/^[A-Za-z0-9_-]{22}$/);
  });

  it("is signed with a raw 64-byte r||s that WebCrypto verifies against the embedded key", async () => {
    const k = await key();
    const proof = await createProof(k, { htm: "GET", htu: "https://a.example/p?q=1" });
    const d = decodeProof(proof);
    expect(d.signature.length).toBe(64);
    expect(await verifyProof(proof)).toBe(true);
    // And with the key itself, not just the copy in the header.
    expect(
      await crypto.subtle.verify(
        { name: "ECDSA", hash: "SHA-256" },
        k.keyPair.publicKey,
        d.signature,
        utf8(d.signingInput),
      ),
    ).toBe(true);
  });

  it("does not verify once a claim is changed", async () => {
    const k = await key();
    const d = decodeProof(await createProof(k, { htm: "GET", htu: "https://a.example/" }));
    const forged = `${d.signingInput.split(".")[0]}.${base64urlEncode(utf8(JSON.stringify({ ...d.claims, htm: "POST" })))}`;
    expect(await verifyProof(`${forged}.${base64urlEncode(d.signature)}`)).toBe(false);
  });

  it("does not verify under another key", async () => {
    const [a, b] = await Promise.all([key(), key()]);
    const d = decodeProof(
      await signClaims(a, { jti: "j", htm: "GET", htu: "https://a.example/", iat: 1 }),
    );
    expect(
      await crypto.subtle.verify(
        { name: "ECDSA", hash: "SHA-256" },
        b.keyPair.publicKey,
        d.signature,
        utf8(d.signingInput),
      ),
    ).toBe(false);
  });

  it("refuses an empty method or a bad htu", async () => {
    const k = await key();
    await expect(createProof(k, { htm: "", htu: "https://a.example/" })).rejects.toThrow(/method/);
    await expect(createProof(k, { htm: "GET", htu: "/relative" })).rejects.toThrow(/htu/);
  });
});

describe("normalizeHTU", () => {
  // The same table as go/lib/dpop TestNormalizeHTU, so both sides agree.
  it("normalises like the Go verifier", () => {
    const cases: Record<string, string> = {
      "https://a.example/p": "https://a.example/p",
      "https://a.example": "https://a.example/",
      "http://a.example:80/p": "http://a.example/p",
      "https://a.example:8443/p?q": "https://a.example:8443/p",
      "https://[::1]:443/p": "https://[::1]/p",
      "https://a.example/a%2Fb#top": "https://a.example/a%2Fb",
      "HTTPS://API.Example.com:443/r?x=y#f": "https://api.example.com/r",
    };
    for (const [input, want] of Object.entries(cases))
      expect(normalizeHTU(input), input).toBe(want);
  });

  it("refuses what the Go verifier refuses", () => {
    for (const bad of [
      "/p",
      "ftp://a/p",
      "https://user@a/p",
      "https:///p",
      "::",
      "http://:",
      "https://:443/p",
      "https:a/p",
    ]) {
      expect(() => normalizeHTU(bad), bad).toThrow(/htu/);
    }
  });
});
