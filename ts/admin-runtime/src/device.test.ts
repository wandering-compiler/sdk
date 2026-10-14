import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AdminApiError, apiGet, apiPost } from "./api";
import {
  authHeader,
  clearToken,
  configureAuth,
  deviceFetch,
  deviceIdentity,
  getToken,
  onDeviceRevoked,
  setToken,
} from "./auth";
import {
  accessTokenHash,
  base64urlDecode,
  dpopKey,
  generateKeyPair,
  jwkThumbprint,
  MemoryKeyStore,
  type KeyStore,
} from "./dpop";
import {
  challengeOf,
  consumeRedirect,
  exchangeCode,
  FederatedSignInError,
  federatedStartURL,
} from "./oauth";
import type { AdminAuthSpec } from "./types";
import { IDBFactory } from "fake-indexeddb";

// Device identity (DPoP, RFC 9449) in the admin runtime: what the SPA sends
// when `auth.dpop` is on, what it does with the refusals, where it keeps the
// token, and the federated sign-in's code exchange. The server side is a
// scripted fetch that reads every proof the way a verifier does.

const PAGE = "http://localhost:3000";

const BEARER: AdminAuthSpec = {
  login_endpoint: "/admin/api/authenticate",
  whoami_endpoint: "/admin/api/whoami",
};
const DPOP: AdminAuthSpec = { ...BEARER, dpop: true };
const EXCHANGE = "/api/v1/auth/oauth/exchange";

// --- an independent reading of a proof ---------------------------------

interface Proof {
  header: { typ?: string; alg?: string; jwk: JsonWebKey };
  claims: { htm?: string; htu?: string; ath?: string; jti?: string };
}

const dec = new TextDecoder();

async function readProof(raw: string): Promise<Proof> {
  const [h, p, s] = raw.split(".");
  const header = JSON.parse(dec.decode(base64urlDecode(h))) as Proof["header"];
  const claims = JSON.parse(dec.decode(base64urlDecode(p))) as Proof["claims"];
  const pub = await crypto.subtle.importKey(
    "jwk",
    header.jwk,
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["verify"],
  );
  const ok = await crypto.subtle.verify(
    { name: "ECDSA", hash: "SHA-256" },
    pub,
    new Uint8Array(base64urlDecode(s)),
    new TextEncoder().encode(`${h}.${p}`),
  );
  if (!ok) throw new Error("proof signature does not verify");
  return { header, claims };
}

interface Sent {
  url: string;
  method: string;
  auth: string | null;
  proof: Proof | null;
  thumbprint: string | null;
  body: string;
}

type Reply = (s: Sent, n: number) => Response | Promise<Response>;

function server(reply: Reply = () => json(200, {})) {
  const sent: Sent[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    // A Bearer call passes the endpoint as given (relative); resolve it
    // the way a browser would.
    const req =
      input instanceof Request && init === undefined
        ? input
        : new Request(typeof input === "string" ? new URL(input, PAGE) : input, init);
    const raw = req.headers.get("DPoP");
    const proof = raw ? await readProof(raw) : null;
    const s: Sent = {
      url: req.url,
      method: req.method,
      auth: req.headers.get("Authorization"),
      proof,
      thumbprint: proof ? await jwkThumbprint(proof.header.jwk as Record<string, unknown>) : null,
      body: await req.text(),
    };
    sent.push(s);
    return reply(s, sent.length);
  });
  vi.stubGlobal("fetch", fn);
  return { fn, sent };
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function refusal(code: "DEVICE_REVOKED" | "SESSION_EXPIRED"): Response {
  return json(401, {
    error: { code: "UNAUTHENTICATED", message: code.toLowerCase(), details: [{ code }] },
  });
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

async function storedThumbprint(store: KeyStore): Promise<string | undefined> {
  const pair = await store.load();
  return pair ? (await dpopKey(pair)).thumbprint : undefined;
}

// Another tab on the same profile: it replaced the device key in the shared
// store and wrote its token, which reaches this tab as a storage event.
async function otherTabSignsIn(store: KeyStore, token: string): Promise<string> {
  const pair = await generateKeyPair();
  await store.save(pair);
  window.localStorage.setItem("w17_admin_token", token);
  window.dispatchEvent(
    new StorageEvent("storage", {
      key: "w17_admin_token",
      newValue: token,
      storageArea: window.localStorage,
    }),
  );
  return (await dpopKey(pair)).thumbprint;
}

let store: MemoryKeyStore;

beforeEach(() => {
  store = new MemoryKeyStore();
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.history.replaceState(null, "", "/admin/");
});

afterEach(() => {
  configureAuth(BEARER);
  vi.unstubAllGlobals();
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.history.replaceState(null, "", "/admin/");
});

describe("device identity off", () => {
  it("sends Bearer, no proof, to the endpoint as given — the request it always was", async () => {
    configureAuth(BEARER);
    setToken("tok");
    const { fn, sent } = server();
    await apiGet("/admin/api/whoami");
    expect(fn).toHaveBeenCalledWith("/admin/api/whoami", {
      method: "GET",
      headers: { "Content-Type": "application/json", Authorization: "Bearer tok" },
      body: undefined,
    });
    expect(sent[0].proof).toBeNull();
    expect(deviceIdentity()).toBe(false);
  });

  it("ignores the public-computer choice: the token goes to localStorage", () => {
    configureAuth(BEARER);
    setToken("tok", { publicComputer: true });
    expect(window.localStorage.getItem("w17_admin_token")).toBe("tok");
    expect(window.sessionStorage.getItem("w17_admin_token")).toBeNull();
  });

  it("is what configureAuth returns to after device identity was on", async () => {
    configureAuth(DPOP, { keyStore: store });
    configureAuth(BEARER);
    setToken("tok");
    const { sent } = server();
    await apiGet("/x");
    expect(sent[0].auth).toBe("Bearer tok");
    expect(sent[0].proof).toBeNull();
  });
});

describe("device identity on", () => {
  beforeEach(() => configureAuth(DPOP, { keyStore: store }));

  it("signs a request: Authorization DPoP and a proof for its method, URL and token", async () => {
    setToken("tok");
    const { sent } = server();
    await apiGet("/admin/api/whoami?x=1");
    const s = sent[0];
    expect(s.auth).toBe("DPoP tok");
    expect(s.proof?.header.typ).toBe("dpop+jwt");
    expect(s.proof?.claims.htm).toBe("GET");
    expect(s.proof?.claims.htu).toBe(`${PAGE}/admin/api/whoami`);
    expect(s.proof?.claims.ath).toBe(await accessTokenHash("tok"));
    expect(s.thumbprint).toBe(await storedThumbprint(store));
  });

  it("never sends Bearer", () => {
    setToken("tok");
    expect(authHeader()).toBeUndefined();
  });

  it("sends a request without a token and not a mint with no DPoP headers", async () => {
    const { sent } = server();
    await apiGet("/admin/api/whoami");
    expect(sent[0].auth).toBeNull();
    expect(sent[0].proof).toBeNull();
  });

  it("makes a sign-in a mint call: an unbound proof and no token, even a stored one", async () => {
    setToken("stale");
    const { sent } = server(() => json(200, { token: "fresh" }));
    await apiPost("/admin/api/authenticate", { username: "a", password: "b" }, undefined, {
      mint: true,
    });
    expect(sent[0].auth).toBeNull();
    expect(sent[0].proof?.claims.htm).toBe("POST");
    expect(sent[0].proof?.claims.ath).toBeUndefined();
    // The flag is the call's alone: the next request carries the token again.
    await apiGet("/admin/api/whoami");
    expect(sent[1].auth).toBe("DPoP stale");
  });

  it("refuses to send the token to another origin", async () => {
    setToken("tok");
    const { fn } = server();
    await expect(apiGet("https://elsewhere.example/x")).rejects.toThrow(/not in the allowed/);
    expect(fn).not.toHaveBeenCalled();
  });

  it("signs for the exchange endpoint's origin when it is another", async () => {
    configureAuth(
      { ...DPOP, oauth_exchange_endpoint: "https://api.example/v1/auth/oauth/exchange" },
      { keyStore: store },
    );
    const { sent } = server(() => json(200, { token: "t" }));
    await apiPost("https://api.example/v1/auth/oauth/exchange", {}, undefined, { mint: true });
    expect(sent[0].proof?.claims.htu).toBe("https://api.example/v1/auth/oauth/exchange");
  });

  it("throws when used before configureAuth turned it on", async () => {
    configureAuth(BEARER);
    await expect(deviceFetch(`${PAGE}/x`, {})).rejects.toThrow(/not configured/);
  });
});

describe("DEVICE_REVOKED", () => {
  beforeEach(() => configureAuth(DPOP, { keyStore: store }));

  it("ends the session once, discards the key, and tells the app", async () => {
    setToken("tok");
    server((_, n) => (n === 1 ? json(200, {}) : refusal("DEVICE_REVOKED")));
    await apiGet("/warm");
    const before = await storedThumbprint(store);
    expect(before).toBeDefined();
    const told = vi.fn();
    const off = onDeviceRevoked(told);
    const results = await Promise.allSettled([apiGet("/a"), apiGet("/b")]);
    off();
    for (const r of results) {
      expect(r.status).toBe("rejected");
      expect((r as PromiseRejectedResult).reason).toBeInstanceOf(AdminApiError);
      expect(((r as PromiseRejectedResult).reason as AdminApiError).status).toBe(401);
    }
    expect(told).toHaveBeenCalledTimes(1);
    expect(getToken()).toBeNull();
    expect(window.localStorage.getItem("w17_admin_token")).toBeNull();
    const after = await storedThumbprint(store);
    expect(after).not.toBe(before);
  });

  it("does not end a session the user signed in to after the refused request was sent", async () => {
    setToken("old");
    const told = vi.fn();
    const off = onDeviceRevoked(told);
    const gate = deferred<void>();
    server(async () => {
      await gate.promise;
      return refusal("DEVICE_REVOKED");
    });
    const inFlight = apiGet("/a").catch((e: unknown) => e);
    await vi.waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    setToken("new");
    gate.resolve();
    expect(await inFlight).toBeInstanceOf(AdminApiError);
    off();
    expect(told).not.toHaveBeenCalled();
    expect(getToken()).toBe("new");
  });

  it("does not end a session another tab signed in to meanwhile", async () => {
    setToken("old");
    const told = vi.fn();
    const off = onDeviceRevoked(told);
    const gate = deferred<void>();
    server(async () => {
      await gate.promise;
      return refusal("DEVICE_REVOKED");
    });
    const inFlight = apiGet("/a").catch((e: unknown) => e);
    await vi.waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    await otherTabSignsIn(store, "theirs");
    gate.resolve();
    await inFlight;
    off();
    expect(told).not.toHaveBeenCalled();
    expect(getToken()).toBe("theirs");
  });

  it("re-sends a password sign-in once, on a fresh key, when the stored key was revoked", async () => {
    await apiGet("/warm").catch(() => undefined); // no token: no key yet
    setToken("old");
    const told = vi.fn();
    const off = onDeviceRevoked(told);
    const { sent } = server((_, n) =>
      n === 1 ? refusal("DEVICE_REVOKED") : json(200, { token: "t" }),
    );
    const resp = await apiPost<{ token: string }>(
      "/admin/api/authenticate",
      { username: "a", password: "b" },
      undefined,
      { mint: true, resign: true },
    );
    off();
    expect(resp.token).toBe("t");
    expect(sent).toHaveLength(2);
    expect(sent[0].thumbprint).not.toBe(sent[1].thumbprint);
    expect(sent[1].body).toBe(sent[0].body);
    expect(sent[1].thumbprint).toBe(await storedThumbprint(store));
    // A sign-in is not a session being refused.
    expect(told).not.toHaveBeenCalled();
  });

  it("sends a one-time mint (the code exchange) once, and discards the key", async () => {
    const { sent } = server(() => refusal("DEVICE_REVOKED"));
    await expect(
      apiPost(EXCHANGE, { code: "c" }, undefined, { mint: true }),
    ).rejects.toBeInstanceOf(AdminApiError);
    expect(sent).toHaveLength(1);
    expect(await storedThumbprint(store)).toBeUndefined();
  });

  it("a listener that throws does not mask the refusal", async () => {
    setToken("tok");
    const off = onDeviceRevoked(() => {
      throw new Error("boom");
    });
    server(() => refusal("DEVICE_REVOKED"));
    await expect(apiGet("/a")).rejects.toBeInstanceOf(AdminApiError);
    off();
  });
});

describe("SESSION_EXPIRED", () => {
  it("is the 401 it is: the key stays and nobody is told the device is gone", async () => {
    configureAuth(DPOP, { keyStore: store });
    setToken("tok");
    const told = vi.fn();
    const off = onDeviceRevoked(told);
    server(() => refusal("SESSION_EXPIRED"));
    await expect(apiGet("/a")).rejects.toMatchObject({ status: 401 });
    off();
    expect(told).not.toHaveBeenCalled();
    expect(await storedThumbprint(store)).toBeDefined();
  });
});

describe("where the token lives", () => {
  it("public computer: sessionStorage only, and the profile's token is removed", () => {
    configureAuth(DPOP, { keyStore: store });
    window.localStorage.setItem("w17_admin_token", "profile");
    setToken("pub", { publicComputer: true });
    expect(window.sessionStorage.getItem("w17_admin_token")).toBe("pub");
    expect(window.localStorage.getItem("w17_admin_token")).toBeNull();
    expect(getToken()).toBe("pub");
    clearToken();
    expect(getToken()).toBeNull();
    expect(window.sessionStorage.getItem("w17_admin_token")).toBeNull();
  });

  it("a normal sign-in replaces a public-computer one in this tab", () => {
    configureAuth(DPOP, { keyStore: store });
    setToken("pub", { publicComputer: true });
    setToken("normal");
    expect(window.sessionStorage.getItem("w17_admin_token")).toBeNull();
    expect(window.localStorage.getItem("w17_admin_token")).toBe("normal");
  });

  it("a reload in the same tab restores the public-computer session", () => {
    window.sessionStorage.setItem("w17_admin_token", "pub");
    window.localStorage.setItem("w17_admin_token", "profile");
    configureAuth(DPOP, { keyStore: store });
    expect(getToken()).toBe("pub");
  });

  it("another tab's sign-in: this tab signs with the key the store holds now", async () => {
    configureAuth(DPOP, { keyStore: store });
    setToken("mine");
    const { sent } = server();
    await apiGet("/a");
    const theirs = await otherTabSignsIn(store, "theirs");
    await apiGet("/b");
    expect(sent[0].thumbprint).not.toBe(theirs);
    expect(sent[1].thumbprint).toBe(theirs);
    expect(sent[1].auth).toBe("DPoP theirs");
  });

  it("a sign-in makes this tab sign with the key the store holds", async () => {
    configureAuth(DPOP, { keyStore: store });
    const { sent } = server(() => json(200, { token: "t" }));
    await apiGet("/warm");
    setToken("old");
    await apiGet("/a");
    // Another tab replaced the key; this tab is handed a token for it.
    const theirs = await store.save(await generateKeyPair()).then(() => storedThumbprint(store));
    setToken("t");
    await apiGet("/b");
    expect(sent[1].thumbprint).not.toBe(theirs);
    expect(sent[2].thumbprint).toBe(theirs);
  });

  it("a public-computer session follows no other tab", async () => {
    configureAuth(DPOP, { keyStore: store });
    setToken("pub", { publicComputer: true });
    const { sent } = server();
    await apiGet("/a");
    await otherTabSignsIn(store, "theirs");
    await apiGet("/b");
    expect(sent[1].thumbprint).toBe(sent[0].thumbprint);
    expect(sent[1].auth).toBe("DPoP pub");
  });

  it("ignores storage events for other keys and other storage areas", async () => {
    configureAuth(DPOP, { keyStore: store });
    setToken("mine");
    const { sent } = server();
    await apiGet("/a");
    await store.save(await generateKeyPair());
    window.dispatchEvent(
      new StorageEvent("storage", {
        key: "other",
        newValue: "x",
        storageArea: window.localStorage,
      }),
    );
    window.dispatchEvent(
      new StorageEvent("storage", {
        key: "w17_admin_token",
        newValue: "x",
        storageArea: window.sessionStorage,
      }),
    );
    await apiGet("/b");
    expect(sent[1].thumbprint).toBe(sent[0].thumbprint);
  });

  it("an IndexedDB that fails keeps the key, and every later token, in this tab's memory", async () => {
    vi.stubGlobal("indexedDB", {
      open: () => {
        throw new Error("blocked");
      },
    });
    configureAuth(DPOP);
    window.localStorage.setItem("w17_admin_token", "profile");
    const { sent } = server(() => json(200, { token: "t" }));
    await apiPost("/admin/api/authenticate", {}, undefined, { mint: true });
    expect(sent[0].proof).not.toBeNull();
    setToken("t");
    expect(getToken()).toBe("t");
    // The profile's token belongs to the other tabs: left where it is.
    expect(window.localStorage.getItem("w17_admin_token")).toBe("profile");
    expect(window.sessionStorage.getItem("w17_admin_token")).toBeNull();
    // Another tab's sign-in is bound to a key this tab does not hold.
    window.dispatchEvent(
      new StorageEvent("storage", {
        key: "w17_admin_token",
        newValue: "theirs",
        storageArea: window.localStorage,
      }),
    );
    expect(getToken()).toBe("t");
    clearToken();
    expect(getToken()).toBeNull();
    expect(window.localStorage.getItem("w17_admin_token")).toBe("profile");
  });

  it("an IndexedDB that fails mid-session keeps this tab's session, and its sign-out clears it", async () => {
    const real = new IDBFactory();
    let broken = false;
    vi.stubGlobal("indexedDB", {
      open: (name: string, version?: number) => {
        if (broken) throw new Error("blocked");
        return real.open(name, version);
      },
    });
    configureAuth(DPOP);
    const { sent } = server(() => json(200, { token: "mine" }));
    await apiPost("/admin/api/authenticate", {}, undefined, { mint: true });
    setToken("mine"); // reloads the key: the next request reads IndexedDB again
    broken = true;
    await apiGet("/a");
    // The fallback is seeded with this tab's key, which "mine" is bound to.
    expect(sent[1].thumbprint).toBe(sent[0].thumbprint);
    expect(sent[1].auth).toBe("DPoP mine");
    expect(getToken()).toBe("mine");
    clearToken();
    expect(getToken()).toBeNull();
    expect(window.localStorage.getItem("w17_admin_token")).toBeNull();
  });

  it("a fallen-back tab's sign-out leaves a token another tab wrote since", async () => {
    const real = new IDBFactory();
    let broken = false;
    vi.stubGlobal("indexedDB", {
      open: (name: string, version?: number) => {
        if (broken) throw new Error("blocked");
        return real.open(name, version);
      },
    });
    configureAuth(DPOP);
    server(() => json(200, {}));
    await apiPost("/admin/api/authenticate", {}, undefined, { mint: true });
    setToken("mine");
    broken = true;
    await apiGet("/a");
    window.localStorage.setItem("w17_admin_token", "theirs");
    clearToken();
    expect(window.localStorage.getItem("w17_admin_token")).toBe("theirs");
  });

  it("no IndexedDB at all: the key is this tab's, and so is every token", () => {
    vi.stubGlobal("indexedDB", undefined);
    window.localStorage.setItem("w17_admin_token", "theirs");
    configureAuth(DPOP);
    expect(getToken()).toBeNull();
    setToken("mine");
    expect(getToken()).toBe("mine");
    expect(window.localStorage.getItem("w17_admin_token")).toBe("theirs");
  });

  it("a tab whose IndexedDB fails neither signs with nor clears the other tabs' token", async () => {
    vi.stubGlobal("indexedDB", {
      open: () => {
        throw new Error("blocked");
      },
    });
    window.localStorage.setItem("w17_admin_token", "shared");
    configureAuth(DPOP);
    const { sent } = server(() => json(401, { error: { code: "UNAUTHENTICATED", message: "x" } }));
    await expect(apiGet("/a")).rejects.toMatchObject({ status: 401 });
    // The first request loaded the key, and only then could the tab know.
    await expect(apiGet("/b")).rejects.toMatchObject({ status: 401 });
    expect(sent[1].auth).toBeNull();
    expect(getToken()).toBeNull();
    clearToken();
    expect(window.localStorage.getItem("w17_admin_token")).toBe("shared");
    setToken("mine");
    expect(window.localStorage.getItem("w17_admin_token")).toBe("shared");
    await apiGet("/c").catch(() => undefined);
    expect(sent[2].auth).toBe("DPoP mine");
  });
});

describe("federated sign-in: the code flow", () => {
  const PKCE: AdminAuthSpec = { ...DPOP, oauth_exchange_endpoint: EXCHANGE };

  it("adds an S256 challenge to the start URL and keeps its verifier for the exchange", async () => {
    configureAuth(PKCE, { keyStore: store });
    const url = new URL(
      await federatedStartURL("/api/v1/auth/oauth/acme/authorize?redirect_after=/admin", {
        publicComputer: true,
      }),
    );
    expect(url.origin).toBe(PAGE);
    expect(url.searchParams.get("redirect_after")).toBe("/admin");
    const challenge = url.searchParams.get("code_challenge");

    const { sent } = server(() => json(200, { token: "t", public_computer: true }));
    await exchangeCode(PKCE, "the-code");
    const body = JSON.parse(sent[0].body) as Record<string, unknown>;
    expect(body.code).toBe("the-code");
    expect(body.public_computer).toBe(true);
    expect(typeof body.code_verifier).toBe("string");
    expect((body.code_verifier as string).length).toBe(43);
    expect(await challengeOf(body.code_verifier as string)).toBe(challenge);
    // A mint call: an unbound proof, no token.
    expect(sent[0].auth).toBeNull();
    expect(sent[0].proof?.claims.htu).toBe(`${PAGE}${EXCHANGE}`);
    expect(sent[0].proof?.claims.ath).toBeUndefined();
    // The session it returns, kept as a public-computer one.
    expect(window.sessionStorage.getItem("w17_admin_token")).toBe("t");
    expect(window.localStorage.getItem("w17_admin_token")).toBeNull();
  });

  it("keeps a public-computer session tab-only even when the answer does not echo it", async () => {
    configureAuth(PKCE, { keyStore: store });
    await federatedStartURL("/start", { publicComputer: true });
    server(() => json(200, { token: "t" }));
    await exchangeCode(PKCE, "c");
    expect(window.sessionStorage.getItem("w17_admin_token")).toBe("t");
    expect(window.localStorage.getItem("w17_admin_token")).toBeNull();
  });

  it("sends no public_computer the user did not ask for", async () => {
    configureAuth(PKCE, { keyStore: store });
    await federatedStartURL("/start");
    const { sent } = server(() => json(200, { token: "t" }));
    await exchangeCode(PKCE, "c");
    expect(JSON.parse(sent[0].body)).not.toHaveProperty("public_computer");
    expect(window.localStorage.getItem("w17_admin_token")).toBe("t");
  });

  it("uses the verifier once", async () => {
    configureAuth(PKCE, { keyStore: store });
    await federatedStartURL("/start");
    server(() => json(200, { token: "t" }));
    await exchangeCode(PKCE, "c");
    await expect(exchangeCode(PKCE, "c")).rejects.toBeInstanceOf(FederatedSignInError);
  });

  it("refuses a code this tab holds no verifier for, without sending it", async () => {
    configureAuth(PKCE, { keyStore: store });
    const { fn } = server();
    await expect(exchangeCode(PKCE, "c")).rejects.toBeInstanceOf(FederatedSignInError);
    window.sessionStorage.setItem("w17_admin_oauth", "not json");
    await expect(exchangeCode(PKCE, "c")).rejects.toBeInstanceOf(FederatedSignInError);
    expect(fn).not.toHaveBeenCalled();
  });

  it("refuses an exchange without an endpoint, or whose answer has no token", async () => {
    configureAuth(PKCE, { keyStore: store });
    await expect(exchangeCode(DPOP, "c")).rejects.toThrow(/no sign-in code exchange/);
    await federatedStartURL("/start");
    server(() => json(200, {}));
    await expect(exchangeCode(PKCE, "c")).rejects.toThrow(/missing 'token'/);
    expect(getToken()).toBeNull();
  });

  it("takes #code= out of the URL for the exchange", () => {
    configureAuth(PKCE, { keyStore: store });
    window.history.replaceState(null, "", "/admin/#code=abc&next=x");
    expect(consumeRedirect(PKCE)).toEqual({ kind: "code", code: "abc" });
    expect(window.location.href).not.toContain("abc");
    expect(window.location.hash).toContain("next=x");
  });

  it("drops a #token= on a device-binding admin: it is bound to no key", () => {
    configureAuth(PKCE, { keyStore: store });
    window.history.replaceState(null, "", "/admin/#token=ey.leaked");
    expect(consumeRedirect(PKCE)).toEqual({ kind: "token-refused" });
    expect(getToken()).toBeNull();
    expect(window.location.href).not.toContain("ey.leaked");
  });

  it("sends no stored Bearer token with a code exchange", async () => {
    const bearer = { ...BEARER, oauth_exchange_endpoint: EXCHANGE };
    configureAuth(bearer);
    setToken("tok");
    await federatedStartURL("/start");
    const { sent } = server(() => json(200, { token: "t" }));
    await exchangeCode(bearer, "c");
    expect(sent[0].auth).toBeNull();
    expect(sent[0].proof).toBeNull();
    expect(getToken()).toBe("t");
  });

  it("still stores a #token= on a Bearer admin", () => {
    const bearer = { ...BEARER, oauth_exchange_endpoint: EXCHANGE };
    configureAuth(bearer);
    window.history.replaceState(null, "", "/admin/#token=ey.ok");
    expect(consumeRedirect(bearer)).toEqual({ kind: "token" });
    expect(getToken()).toBe("ey.ok");
  });

  it("leaves #code= alone on an admin without an exchange endpoint", () => {
    configureAuth(BEARER);
    window.history.replaceState(null, "", "/admin/#code=abc");
    expect(consumeRedirect(BEARER)).toEqual({ kind: "none" });
    expect(window.location.hash).toBe("#code=abc");
  });
});
