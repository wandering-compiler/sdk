// base64url without padding (RFC 4648 §5, as JOSE uses it: RFC 7515 §2).
// Written out rather than built on btoa/atob so that it behaves the same in
// every runtime and never goes through a binary string.

const ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

const DECODE: Record<string, number> = {};
for (let i = 0; i < ALPHABET.length; i++) DECODE[ALPHABET.charAt(i)] = i;

/** Encodes bytes as base64url with no padding. */
export function base64urlEncode(bytes: Uint8Array): string {
  let out = "";
  let i = 0;
  for (; i + 2 < bytes.length; i += 3) {
    const n = (bytes[i]! << 16) | (bytes[i + 1]! << 8) | bytes[i + 2]!;
    out +=
      ALPHABET.charAt(n >>> 18) +
      ALPHABET.charAt((n >>> 12) & 63) +
      ALPHABET.charAt((n >>> 6) & 63) +
      ALPHABET.charAt(n & 63);
  }
  const rest = bytes.length - i;
  if (rest === 1) {
    const n = bytes[i]! << 16;
    out += ALPHABET.charAt(n >>> 18) + ALPHABET.charAt((n >>> 12) & 63);
  } else if (rest === 2) {
    const n = (bytes[i]! << 16) | (bytes[i + 1]! << 8);
    out +=
      ALPHABET.charAt(n >>> 18) +
      ALPHABET.charAt((n >>> 12) & 63) +
      ALPHABET.charAt((n >>> 6) & 63);
  }
  return out;
}

/**
 * Decodes unpadded base64url. Throws on padding, a character outside the
 * alphabet, an impossible length, or non-zero trailing bits (a non-canonical
 * encoding), so one value has exactly one accepted spelling.
 */
export function base64urlDecode(s: string): Uint8Array {
  if (s.length % 4 === 1) throw new Error("base64url: invalid length");
  const out = new Uint8Array(Math.floor((s.length * 3) / 4));
  let o = 0;
  let acc = 0;
  let bits = 0;
  for (const ch of s) {
    const v = DECODE[ch];
    if (v === undefined) throw new Error("base64url: invalid character");
    acc = (acc << 6) | v;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[o++] = (acc >>> bits) & 0xff;
    }
  }
  if ((acc & ((1 << bits) - 1)) !== 0) throw new Error("base64url: non-canonical trailing bits");
  return out;
}

const encoder = new TextEncoder();

/** UTF-8 bytes of a string. */
export function utf8(s: string): Uint8Array<ArrayBuffer> {
  return encoder.encode(s);
}
