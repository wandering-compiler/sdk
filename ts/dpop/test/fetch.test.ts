import { describe, expect, it, vi } from "vitest";

import {
  createDpopFetch,
  DpopOriginError,
  DpopRefusedError,
  isDeviceRevoked,
  isSessionExpired,
  isUseNonceChallenge,
} from "../src/fetch";
import { MemoryKeyStore, type KeyStore } from "../src/key";
import { accessTokenHash } from "../src/proof";
import { json, scriptedFetch, useNonce, verifyProof, type Reply } from "./helpers";

const ORIGIN = "https://api.example.com";
const ok = () => new Response("ok", { status: 200 });

function setup(reply: Reply = () => ok(), token: string | null = "tok-1") {
  const server = scriptedFetch(reply);
  const store = new MemoryKeyStore();
  const getToken = vi.fn(() => token);
  const f = createDpopFetch({ origins: [ORIGIN], keyStore: store, getToken, fetch: server.fetch });
  return { f, server, store, getToken };
}

describe("createDpopFetch: what a request carries", () => {
  it("attaches Authorization: DPoP and a proof bound to method, URL and token", async () => {
    const { f, server } = setup();
    const resp = await f(`${ORIGIN}/v1/things?page=2#frag`, { method: "PATCH", body: "{}" });
    expect(resp.status).toBe(200);
    const s = server.sent[0]!;
    expect(s.req.headers.get("Authorization")).toBe("DPoP tok-1");
    expect(s.proof!.claims).toMatchObject({
      htm: "PATCH",
      htu: `${ORIGIN}/v1/things`,
      ath: await accessTokenHash("tok-1"),
    });
    expect(s.proof!.claims).not.toHaveProperty("nonce");
    expect(await verifyProof(s.proofRaw!)).toBe(true);
    expect(s.proof!.header.jwk).toEqual((await f.key()).jwk);
    expect(s.body).toBe("{}");
  });

  it("sends no Authorization and no ath without a token", async () => {
    const { f, server } = setup(undefined, null);
    await f(`${ORIGIN}/v1/sign-in`, { method: "POST" });
    const s = server.sent[0]!;
    expect(s.req.headers.has("Authorization")).toBe(false);
    expect(s.proof!.claims).not.toHaveProperty("ath");
  });

  it("does not modify the caller's Request, and refuses redirects", async () => {
    const { f, server } = setup();
    const mine = new Request(`${ORIGIN}/r`, { headers: { "X-Mine": "1" } });
    await f(mine);
    expect(mine.headers.has("DPoP")).toBe(false);
    expect(mine.headers.has("Authorization")).toBe(false);
    expect(server.sent[0]!.req.headers.get("X-Mine")).toBe("1");
    expect(server.sent[0]!.req.redirect).toBe("error");
  });

  it("generates one key for concurrent first requests and keeps it", async () => {
    const { f, server, store } = setup();
    const save = vi.spyOn(store, "save");
    await Promise.all([f(`${ORIGIN}/a`), f(`${ORIGIN}/b`), f(`${ORIGIN}/c`)]);
    await f(`${ORIGIN}/d`);
    expect(save).toHaveBeenCalledTimes(1);
    const jwks = new Set(server.sent.map((s) => JSON.stringify(s.proof!.header.jwk)));
    expect(jwks.size).toBe(1);
    expect((await f.key()).keyPair.privateKey.extractable).toBe(false);
  });

  it("retries the key load after a failing store instead of keeping the failure", async () => {
    const server = scriptedFetch(() => ok());
    let fail = true;
    const inner = new MemoryKeyStore();
    const store: KeyStore = {
      load: () => (fail ? Promise.reject(new Error("store down")) : inner.load()),
      save: (p) => inner.save(p),
      clear: () => inner.clear(),
    };
    const f = createDpopFetch({ origins: [ORIGIN], keyStore: store, fetch: server.fetch });
    await expect(f(`${ORIGIN}/r`)).rejects.toThrow("store down");
    expect(server.sent).toHaveLength(0);
    fail = false;
    expect((await f(`${ORIGIN}/r`)).status).toBe(200);
  });
});

describe("createDpopFetch: origins", () => {
  it("throws for any other origin before sending or reading the token", async () => {
    const { f, server, getToken } = setup();
    for (const url of [
      "https://evil.example/r",
      "https://api.example.com.evil.example/r",
      "http://api.example.com/r",
      "https://api.example.com:8443/r",
      "wss://api.example.com/r",
    ]) {
      await expect(f(url), url).rejects.toBeInstanceOf(DpopOriginError);
    }
    await expect(
      f(new Request("https://evil.example/r", { method: "POST", body: "x" })),
    ).rejects.toBeInstanceOf(DpopOriginError);
    expect(server.sent).toHaveLength(0);
    expect(getToken).not.toHaveBeenCalled();
  });

  it("matches origins after normalisation", async () => {
    const server = scriptedFetch(() => ok());
    const f = createDpopFetch({
      origins: ["HTTPS://API.example.com:443/"],
      keyStore: new MemoryKeyStore(),
      fetch: server.fetch,
    });
    await f("https://api.example.com/r");
    await f(new URL("https://API.EXAMPLE.COM/r"));
    expect(server.sent).toHaveLength(2);
  });

  it("refuses a configuration without a valid origin", () => {
    const keyStore = new MemoryKeyStore();
    for (const origins of [
      [],
      ["https://api.example.com/v1"],
      ["api.example.com"],
      ["https://api.example.com?x"],
      ["ftp://a.example"],
      ["https://u@a.example"],
    ]) {
      expect(() => createDpopFetch({ origins, keyStore }), JSON.stringify(origins)).toThrow();
    }
  });

  it("refuses a no-cors request, which would drop the proof", async () => {
    const { f, server } = setup();
    await expect(f(`${ORIGIN}/r`, { mode: "no-cors" })).rejects.toThrow(/no-cors/);
    expect(server.sent).toHaveLength(0);
  });
});

describe("createDpopFetch: nonces", () => {
  it("retries a use_dpop_nonce refusal once, with the new nonce and the same body", async () => {
    const { f, server } = setup((_req, n) => (n === 1 ? useNonce("n-1") : ok()));
    const resp = await f(`${ORIGIN}/r`, { method: "POST", body: "payload" });
    expect(resp.status).toBe(200);
    expect(server.sent).toHaveLength(2);
    const [first, retry] = server.sent as [(typeof server.sent)[0], (typeof server.sent)[0]];
    expect(first.proof!.claims).not.toHaveProperty("nonce");
    expect(retry.proof!.claims.nonce).toBe("n-1");
    expect(retry.proof!.claims.jti).not.toBe(first.proof!.claims.jti);
    expect(retry.body).toBe("payload");
    expect(retry.req.headers.get("Authorization")).toBe("DPoP tok-1");
  });

  it("re-sends a streamed body on the retry", async () => {
    const { f, server } = setup((_req, n) => (n === 1 ? useNonce("n-1") : ok()));
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(new TextEncoder().encode("chunk-1,"));
        c.enqueue(new TextEncoder().encode("chunk-2"));
        c.close();
      },
    });
    const resp = await f(`${ORIGIN}/upload`, {
      method: "PUT",
      body,
      duplex: "half",
    } as RequestInit);
    expect(resp.status).toBe(200);
    expect(server.sent.map((s) => s.body)).toEqual(["chunk-1,chunk-2", "chunk-1,chunk-2"]);
  });

  it("retries exactly once against a server that keeps asking", async () => {
    const { f, server } = setup((_req, n) => useNonce(`n-${n}`));
    const resp = await f(`${ORIGIN}/r`);
    expect(resp.status).toBe(401);
    expect(server.sent).toHaveLength(2);
    // The second refusal's nonce is still learned.
    await f(`${ORIGIN}/r`);
    expect(server.sent[2]!.proof!.claims.nonce).toBe("n-2");
  });

  it("does not retry without a challenge, without a nonce, or with the nonce it just used", async () => {
    const plain401 = json(401, { error: { code: "UNAUTHENTICATED" } }, { "DPoP-Nonce": "n-1" });
    const noNonce = json(401, {}, { "WWW-Authenticate": 'DPoP error="use_dpop_nonce"' });
    for (const reply of [() => plain401.clone(), () => noNonce.clone()]) {
      const { f, server } = setup(reply);
      expect((await f(`${ORIGIN}/r`)).status).toBe(401);
      expect(server.sent).toHaveLength(1);
    }
    // The refusal names the nonce the proof already carried: a retry cannot help.
    const { f, server } = setup((_req, n) =>
      n === 1 ? new Response("ok", { headers: { "DPoP-Nonce": "n-1" } }) : useNonce("n-1"),
    );
    await f(`${ORIGIN}/a`);
    expect((await f(`${ORIGIN}/b`)).status).toBe(401);
    expect(server.sent).toHaveLength(2);
    expect(server.sent[1]!.proof!.claims.nonce).toBe("n-1");
  });

  it("learns a nonce from a success and keeps nonces per origin", async () => {
    const other = "https://files.example.com";
    const server = scriptedFetch(
      (req) =>
        new Response("ok", {
          headers: { "DPoP-Nonce": new URL(req.url).origin === ORIGIN ? "api-n" : "files-n" },
        }),
    );
    const f = createDpopFetch({
      origins: [ORIGIN, other],
      keyStore: new MemoryKeyStore(),
      fetch: server.fetch,
    });
    await f(`${ORIGIN}/a`);
    await f(`${other}/a`);
    await f(`${ORIGIN}/b`);
    await f(`${other}/b`);
    expect(server.sent.map((s) => s.proof!.claims.nonce)).toEqual([
      undefined,
      undefined,
      "api-n",
      "files-n",
    ]);
  });
});

describe("isUseNonceChallenge", () => {
  it("finds the DPoP challenge alone or among others", () => {
    for (const v of [
      'DPoP error="use_dpop_nonce"',
      'dpop error="use_dpop_nonce", algs="ES256"',
      'DPoP algs="ES256 EdDSA", error="use_dpop_nonce"',
      'Bearer realm="x", DPoP error="use_dpop_nonce"',
      "DPoP error=use_dpop_nonce",
    ]) {
      expect(isUseNonceChallenge(v), v).toBe(true);
    }
  });

  it("ignores other errors, other schemes and look-alikes", () => {
    for (const v of [
      null,
      "",
      'DPoP error="invalid_dpop_proof"',
      'Bearer error="use_dpop_nonce"',
      'XDPoP error="use_dpop_nonce"',
      'DPoPx error="use_dpop_nonce"',
    ]) {
      expect(isUseNonceChallenge(v), String(v)).toBe(false);
    }
  });
});

describe("createDpopFetch: refusals", () => {
  const envelope = (code: string) => ({
    error: { code: "UNAUTHENTICATED", message: "refused", details: [{ field: "", code }] },
  });

  it("throws DEVICE_REVOKED as a typed error that can discard the key", async () => {
    const { f, server, store } = setup((_req, n) =>
      n === 1 ? json(401, envelope("DEVICE_REVOKED")) : ok(),
    );
    const before = (await f.key()).thumbprint;
    const err: unknown = await f(`${ORIGIN}/r`).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(DpopRefusedError);
    expect(isDeviceRevoked(err)).toBe(true);
    expect(isSessionExpired(err)).toBe(false);
    const refused = err as DpopRefusedError;
    expect(refused.reason).toBe("DEVICE_REVOKED");
    expect(refused.status).toBe(401);
    // The body is still readable.
    expect(((await refused.response.json()) as { error: { message: string } }).error.message).toBe(
      "refused",
    );

    await refused.discardKey();
    expect(await store.load()).toBeUndefined();
    await f(`${ORIGIN}/r`);
    const after = (await f.key()).thumbprint;
    expect(after).not.toBe(before);
    expect(server.sent[1]!.proof!.header.jwk).toEqual((await f.key()).jwk);
  });

  it("forgets nonces with the key", async () => {
    const { f, server } = setup(() => new Response("ok", { headers: { "DPoP-Nonce": "n-old" } }));
    await f(`${ORIGIN}/a`);
    await f.discardKey();
    await f(`${ORIGIN}/b`);
    expect(server.sent[1]!.proof!.claims).not.toHaveProperty("nonce");
  });

  it("throws SESSION_EXPIRED, from a 403 too", async () => {
    const { f } = setup(() => json(403, envelope("SESSION_EXPIRED")));
    const err: unknown = await f(`${ORIGIN}/r`).catch((e: unknown) => e);
    expect(isSessionExpired(err)).toBe(true);
    expect((err as DpopRefusedError).status).toBe(403);
  });

  it("returns every other refusal as a plain response", async () => {
    for (const resp of [
      () => json(401, envelope("PERMISSION_DENIED")),
      () => json(500, envelope("DEVICE_REVOKED")),
      () => json(401, { error: "DEVICE_REVOKED" }),
      () =>
        new Response("DEVICE_REVOKED", { status: 401, headers: { "Content-Type": "text/plain" } }),
      () =>
        new Response("{not json", { status: 401, headers: { "Content-Type": "application/json" } }),
      () => json(401, null),
    ]) {
      const { f } = setup(resp);
      const r = await f(`${ORIGIN}/r`);
      expect(r.status).toBeGreaterThanOrEqual(401);
    }
  });
});
