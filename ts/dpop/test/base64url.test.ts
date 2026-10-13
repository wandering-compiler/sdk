import { describe, expect, it } from "vitest";

import { base64urlDecode, base64urlEncode, utf8 } from "../src/base64url";

describe("base64url", () => {
  // RFC 4648 §10, unpadded.
  const vectors: [string, string][] = [
    ["", ""],
    ["f", "Zg"],
    ["fo", "Zm8"],
    ["foo", "Zm9v"],
    ["foob", "Zm9vYg"],
    ["fooba", "Zm9vYmE"],
    ["foobar", "Zm9vYmFy"],
  ];

  it("encodes and decodes the RFC 4648 vectors", () => {
    for (const [plain, enc] of vectors) {
      expect(base64urlEncode(utf8(plain))).toBe(enc);
      expect(new TextDecoder().decode(base64urlDecode(enc))).toBe(plain);
    }
  });

  it("uses the URL-safe alphabet", () => {
    expect(base64urlEncode(new Uint8Array([0xfb, 0xff, 0xbf]))).toBe("-_-_");
    expect(Array.from(base64urlDecode("-_-_"))).toEqual([0xfb, 0xff, 0xbf]);
  });

  it("round-trips every length of random bytes", () => {
    for (let n = 0; n < 70; n++) {
      const b = crypto.getRandomValues(new Uint8Array(n));
      const enc = base64urlEncode(b);
      expect(enc).toMatch(/^[A-Za-z0-9_-]*$/);
      expect(Array.from(base64urlDecode(enc))).toEqual(Array.from(b));
    }
  });

  it("refuses padding, foreign characters, impossible lengths and non-canonical bits", () => {
    // "Zh" and "Zm9" carry set bits past the last byte: "Zg" and "Zm8" are
    // the only spellings of "f" and "fo".
    for (const bad of ["Zg==", "Zm9v+", "Zm9v/", "Z", "Zm9vY", "Zh", "Zm9", " Zg"]) {
      expect(() => base64urlDecode(bad), bad).toThrow();
    }
    expect(Array.from(base64urlDecode("Zm8"))).toEqual([0x66, 0x6f]);
  });
});
