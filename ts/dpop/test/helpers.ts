// Test helpers: an independent reading of a proof (split, decode, verify with
// WebCrypto) and a scripted fetch that records what the wrapper sent.

import { base64urlDecode, utf8 } from "../src/base64url";

export interface DecodedProof {
  header: Record<string, unknown>;
  claims: Record<string, unknown>;
  signingInput: string;
  signature: Uint8Array<ArrayBuffer>;
}

const decoder = new TextDecoder();

export function decodeProof(proof: string): DecodedProof {
  const parts = proof.split(".");
  if (parts.length !== 3) throw new Error(`want 3 JWS segments, got ${parts.length}`);
  const [h, p, s] = parts as [string, string, string];
  return {
    header: JSON.parse(decoder.decode(base64urlDecode(h))) as Record<string, unknown>,
    claims: JSON.parse(decoder.decode(base64urlDecode(p))) as Record<string, unknown>,
    signingInput: `${h}.${p}`,
    signature: new Uint8Array(base64urlDecode(s)),
  };
}

/** Verifies an ES256 JWS against the jwk in its own header, the way a DPoP server does. */
export async function verifyProof(proof: string): Promise<boolean> {
  const d = decodeProof(proof);
  const jwk = d.header.jwk as JsonWebKey;
  const pub = await crypto.subtle.importKey(
    "jwk",
    jwk,
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["verify"],
  );
  return crypto.subtle.verify(
    { name: "ECDSA", hash: "SHA-256" },
    pub,
    d.signature,
    utf8(d.signingInput),
  );
}

export interface Sent {
  req: Request;
  body: string;
  proof: DecodedProof | undefined;
  proofRaw: string | null;
}

export type Reply = (req: Request, n: number) => Response | Promise<Response>;

/** A fetch that answers with `reply` and records every request it received. */
export function scriptedFetch(reply: Reply) {
  const sent: Sent[] = [];
  const fn = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = input instanceof Request && init === undefined ? input : new Request(input, init);
    const proofRaw = req.headers.get("DPoP");
    // Read the body itself, as a real fetch does: a request sent twice would
    // fail here the second time.
    const body = await req.text();
    sent.push({
      req,
      body,
      proof: proofRaw ? decodeProof(proofRaw) : undefined,
      proofRaw,
    });
    return reply(req, sent.length);
  };
  return { fetch: fn, sent };
}

export function json(
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

export function useNonce(nonce: string): Response {
  return json(
    401,
    { error: { code: "UNAUTHENTICATED", message: "use a nonce" } },
    { "WWW-Authenticate": 'DPoP error="use_dpop_nonce", algs="ES256 EdDSA"', "DPoP-Nonce": nonce },
  );
}
