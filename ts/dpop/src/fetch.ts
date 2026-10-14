// A fetch wrapper that attaches DPoP proofs, follows server nonces, and
// retries once when the server asks for a fresh one.

import { dpopKey, loadOrCreateKey, type DpopKey, type KeyStore } from "./key";
import { createProof } from "./proof";

/** Header names (RFC 9449 §4.1, §8). */
export const HEADER_PROOF = "DPoP";
export const HEADER_NONCE = "DPoP-Nonce";
export const AUTH_SCHEME = "DPoP";

/**
 * Returns the access token for the next request, or nothing for a request
 * without one: that request carries a proof (with no `ath`) only when it is a
 * mint call ({@link DpopRequestInit.mint}, {@link DpopFetchOptions.mintPaths}),
 * and no DPoP headers otherwise.
 */
export type TokenSource = () => string | null | undefined | Promise<string | null | undefined>;

/** Options of {@link createDpopFetch}. */
export interface DpopFetchOptions {
  /**
   * The origins (`scheme://host[:port]`) the token and proofs may be sent to.
   * Required, with no implicit default: a request to any other origin throws
   * {@link DpopOriginError} before anything is sent or the token is read.
   */
  origins: readonly string[];
  /** Where the device key lives. The key is loaded (or made) on the first request. */
  keyStore: KeyStore;
  /** The access token source. Omitted: no request carries a token. */
  getToken?: TokenSource;
  /**
   * Paths (`/v1/sign-in`, compared with the request URL's path exactly) whose
   * requests are mint calls: without a token they still carry a proof, which
   * the server binds the new credential to. See {@link DpopRequestInit.mint}
   * for marking a single call.
   */
  mintPaths?: readonly string[];
  /** The fetch that sends. Default `globalThis.fetch`. */
  fetch?: typeof fetch;
  /** The clock proofs take iat from, for tests. Default `Date.now`. */
  now?: () => number;
}

/** The init of a {@link DpopFetch} call: fetch's, plus the mint marker. */
export interface DpopRequestInit extends RequestInit {
  /**
   * Marks the call as a mint call (sign-in, token issue): without a token it
   * still carries an unbound proof. A call with neither a token nor this
   * marker (nor a path in {@link DpopFetchOptions.mintPaths}) carries no DPoP
   * headers at all, so anonymous browsing costs no proof and no place in the
   * server's mint replay store.
   */
  mint?: boolean;
}

/** A fetch-compatible function with the device key's lifecycle attached. */
export interface DpopFetch {
  (input: RequestInfo | URL, init?: DpopRequestInit): Promise<Response>;
  /** The device key, loaded or made on first use. Its thumbprint is the device id. */
  key(): Promise<DpopKey>;
  /**
   * Forgets the device key (in the store and in memory) and every nonce, so
   * the next request makes a new key. Call it after a {@link DpopRefusedError}
   * with reason DEVICE_REVOKED, before the user signs in again; that error's
   * own `discardKey` is the safer call, since it discards only the key it was
   * raised for.
   *
   * With `thumbprint`, the STORED key is deleted only when it is the one
   * with that thumbprint: a newer key — made by this page after the refusal,
   * or by another page on the same store (another tab) — is left alone. The
   * key in memory is dropped either way, and the next request loads whatever
   * the store holds, so every page on one store converges on one key.
   * Requests made while a discard is in progress wait for it, so none of
   * them signs with, or reloads, the key being discarded.
   */
  discardKey(thumbprint?: string): Promise<void>;
  /**
   * Forgets the key held in memory, never the stored one: the next request
   * loads the store's. Call it when another page on the same store may have
   * replaced the key — another tab signed in, or answered DEVICE_REVOKED with
   * a new key — so this page signs as the same device the store now holds.
   */
  reloadKey(): void;
}

/** A request to an origin outside {@link DpopFetchOptions.origins}. Nothing was sent. */
export class DpopOriginError extends Error {
  readonly origin: string;
  constructor(origin: string) {
    super(`dpop: origin ${origin} is not in the allowed origins`);
    this.name = "DpopOriginError";
    this.origin = origin;
  }
}

/** Why the server refused a device credential. */
export type RefusalReason = "DEVICE_REVOKED" | "SESSION_EXPIRED";

/**
 * A 401 or 403 whose w17 error envelope names a {@link RefusalReason}:
 *
 *     {"error": {"code": "...", "message": "...", "details": [{"code": "DEVICE_REVOKED", ...}]}}
 *
 * DEVICE_REVOKED: this device's key is no longer accepted; discard it
 * ({@link discardKey}) and sign in again on a new one. SESSION_EXPIRED: the
 * session ended; sign in again (the key stays). The response is attached
 * with its body unread.
 */
export class DpopRefusedError extends Error {
  readonly reason: RefusalReason;
  readonly status: number;
  readonly response: Response;
  /** The thumbprint of the key the refused request was signed with. */
  readonly thumbprint: string;
  private readonly discard: (thumbprint: string) => Promise<void>;

  constructor(
    reason: RefusalReason,
    response: Response,
    thumbprint: string,
    discard: (thumbprint: string) => Promise<void>,
  ) {
    super(`dpop: refused: ${reason} (HTTP ${response.status})`);
    this.name = "DpopRefusedError";
    this.reason = reason;
    this.status = response.status;
    this.response = response;
    this.thumbprint = thumbprint;
    this.discard = discard;
  }

  /**
   * Forgets the key this refusal was raised for ({@link thumbprint}), as
   * {@link DpopFetch.discardKey} does. A key made since then is kept, so a
   * stale error cannot wipe a new device key.
   */
  discardKey(): Promise<void> {
    return this.discard(this.thumbprint);
  }
}

/** True for a {@link DpopRefusedError} with reason DEVICE_REVOKED. */
export function isDeviceRevoked(err: unknown): err is DpopRefusedError {
  return err instanceof DpopRefusedError && err.reason === "DEVICE_REVOKED";
}

/** True for a {@link DpopRefusedError} with reason SESSION_EXPIRED. */
export function isSessionExpired(err: unknown): err is DpopRefusedError {
  return err instanceof DpopRefusedError && err.reason === "SESSION_EXPIRED";
}

/**
 * The normalised origin of a configured value, or undefined when it is not a
 * bare http(s) `scheme://host[:port]` (a trailing `/` is allowed).
 */
function configuredOrigin(o: string): string | undefined {
  let u: URL;
  try {
    u = new URL(o);
  } catch {
    return undefined;
  }
  if (
    (u.protocol !== "http:" && u.protocol !== "https:") ||
    u.username !== "" ||
    u.password !== "" ||
    u.pathname !== "/" ||
    u.search !== "" ||
    u.hash !== "" ||
    /[?#]/.test(o) ||
    !/^[a-z][a-z0-9+.-]*:\/\/[^/]+\/?$/i.test(o)
  ) {
    return undefined;
  }
  return u.origin;
}

/**
 * Whether a WWW-Authenticate value holds a DPoP challenge with
 * error="use_dpop_nonce". Several challenges may share one value (and
 * Headers.get joins repeated headers with ", "), so the DPoP scheme is looked
 * for at the start or after a comma.
 */
export function isUseNonceChallenge(value: string | null): boolean {
  if (!value) return false;
  const l = value.toLowerCase();
  for (let i = l.indexOf("dpop"); i >= 0; i = l.indexOf("dpop", i + 1)) {
    const before = l.slice(0, i).trimEnd();
    const after = l.charAt(i + 4);
    if ((before !== "" && !before.endsWith(",")) || (after !== " " && after !== "")) continue;
    const rest = l.slice(i + 4).replace(/\s+/g, "");
    if (rest.includes('error="use_dpop_nonce"') || rest.includes("error=use_dpop_nonce"))
      return true;
  }
  return false;
}

/** The most of a refusal body {@link refusalReason} reads. */
export const MAX_REFUSAL_BODY = 64 << 10;

/**
 * Reads a body up to `limit` bytes; undefined when it is longer (the rest is
 * cancelled, not read) or unreadable.
 */
async function readCapped(body: ReadableStream<Uint8Array> | null, limit: number) {
  if (!body) return "";
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > limit) {
        // Not awaited: cancelling one branch of a tee'd body settles only
        // once the other branch (the caller's response) is cancelled too.
        void reader.cancel().catch(() => undefined);
        return undefined;
      }
      chunks.push(value);
    }
  } catch {
    return undefined;
  }
  const all = new Uint8Array(size);
  let o = 0;
  for (const c of chunks) {
    all.set(c, o);
    o += c.byteLength;
  }
  return new TextDecoder().decode(all);
}

/**
 * The refusal reason a 401/403 JSON envelope names, if any. Reads at most
 * {@link MAX_REFUSAL_BODY} bytes of a clone; a longer body is not a refusal
 * this client recognises. Never throws.
 */
async function refusalReason(resp: Response): Promise<RefusalReason | undefined> {
  if (resp.status !== 401 && resp.status !== 403) return undefined;
  if (!/\bjson\b/i.test(resp.headers.get("Content-Type") ?? "")) return undefined;
  try {
    const text = await readCapped(resp.clone().body, MAX_REFUSAL_BODY);
    if (text === undefined) return undefined;
    const env = JSON.parse(text) as { error?: { details?: unknown } } | null;
    const details = env?.error?.details;
    if (!Array.isArray(details)) return undefined;
    for (const d of details as unknown[]) {
      const code = (d as { code?: unknown } | null)?.code;
      if (code === "DEVICE_REVOKED" || code === "SESSION_EXPIRED") return code;
    }
  } catch {
    // Not JSON after all: not a refusal this client recognises.
  }
  return undefined;
}

/**
 * Wraps fetch for a DPoP-protected API.
 *
 * For a request to one of `origins` with a token (from `getToken`) it adds
 * `Authorization: DPoP <token>` and a fresh `DPoP` proof whose htu is the
 * request URL without query and fragment; a mint call without a token (`{
 * mint: true }` or a path in `mintPaths`) gets the proof alone; any other
 * request is sent with no DPoP headers, and no key is loaded for it. It remembers the last `DPoP-Nonce`
 * each origin sent, on any response, and puts it in the next proof. A 401
 * with `WWW-Authenticate: DPoP error="use_dpop_nonce"` and a new nonce is
 * retried exactly once, with the same body; a second refusal is returned.
 *
 * The body is re-sent from a clone taken before the first attempt, so any
 * body fetch accepts can be retried, a stream included.
 *
 * Redirects are refused (`redirect: "error"`): a proof is bound to the URL it
 * was made for, so a server at the redirect target would refuse it anyway,
 * and following would hand the token to that target.
 *
 * A request elsewhere throws {@link DpopOriginError}; a refusal naming
 * DEVICE_REVOKED or SESSION_EXPIRED throws {@link DpopRefusedError}. Every
 * other response, a refusal included, is returned as is.
 */
export function createDpopFetch(opts: DpopFetchOptions): DpopFetch {
  const allowed = new Set<string>();
  if (opts.origins.length === 0) throw new Error("dpop: createDpopFetch needs at least one origin");
  for (const o of opts.origins) {
    const n = configuredOrigin(o);
    if (!n) throw new Error(`dpop: origin ${JSON.stringify(o)} is not scheme://host[:port]`);
    allowed.add(n);
  }
  const mintPaths = new Set<string>();
  for (const p of opts.mintPaths ?? []) {
    if (!p.startsWith("/") || /[?#]/.test(p)) {
      throw new Error(`dpop: mint path ${JSON.stringify(p)} is not a path starting with /`);
    }
    mintPaths.add(new URL(p, "http://x").pathname);
  }
  const store = opts.keyStore;
  const send =
    opts.fetch ?? ((input: RequestInfo | URL, init?: RequestInit) => globalThis.fetch(input, init));
  const nonces = new Map<string, string>();
  let keyPromise: Promise<DpopKey> | undefined;
  // Discards run one after another, and a key load starts only after every
  // discard requested before it has finished: a request can never reload the
  // pair a discard is about to delete.
  let discards: Promise<void> = Promise.resolve();

  // The last key this wrapper signed with, kept across reloadKey(): a reload
  // that fails (the store briefly unreadable) goes on with it instead of
  // failing requests made under a live session. A discard forgets it.
  let lastKey: DpopKey | undefined;

  const key = (): Promise<DpopKey> => {
    if (!keyPromise) {
      const p = discards
        .then(() => loadOrCreateKey(store))
        .then(
          (k) => (lastKey = k),
          (err: unknown) => {
            if (lastKey) return lastKey;
            throw err;
          },
        );
      keyPromise = p;
      // A failed load must not stick: the next request tries again.
      p.catch(() => {
        if (keyPromise === p) keyPromise = undefined;
      });
    }
    return keyPromise;
  };

  const discardKey = (thumbprint?: string): Promise<void> => {
    const op = discards.then(async () => {
      if (thumbprint !== undefined) {
        // Compared with the STORE, not with memory: the store is what every
        // page on it loads from. Comparing with this page's own key let a
        // second tab, refused for the old key after the first had already
        // made a new one, delete the first tab's new key — and the two tabs
        // went on as two devices.
        // A store that compares and deletes in one step does so (another
        // tab cannot save between the two); any other is read, then cleared.
        const cleared = store.clearIf ? await store.clearIf(thumbprint) : undefined;
        if (cleared === false) return;
        if (cleared === undefined) {
          const stored = await store.load();
          const held = stored ? await dpopKey(stored).catch(() => undefined) : undefined;
          if (held?.thumbprint !== thumbprint) return;
          await store.clear();
        }
        nonces.clear();
        return;
      }
      nonces.clear();
      await store.clear();
    });
    discards = op.catch(() => undefined);
    // Requests from now on wait for this discard and then load the store's
    // key (see key()) — the newer one when the discard left it alone.
    keyPromise = undefined;
    lastKey = undefined;
    return op;
  };

  const reloadKey = (): void => {
    keyPromise = undefined;
  };

  const attempt = async (
    req: Request,
    k: DpopKey,
    token: string | undefined,
    nonce: string | undefined,
  ) => {
    const out = req.clone();
    const proof = await createProof(k, {
      htm: out.method,
      htu: out.url,
      accessToken: token,
      nonce,
      ...(opts.now ? { now: opts.now } : {}),
    });
    if (token) out.headers.set("Authorization", `${AUTH_SCHEME} ${token}`);
    out.headers.set(HEADER_PROOF, proof);
    return send(out);
  };

  const learn = (origin: string, resp: Response): string | undefined => {
    const fresh = resp.headers.get(HEADER_NONCE) ?? undefined;
    if (fresh) nonces.set(origin, fresh);
    return fresh;
  };

  const dpopFetch = async (input: RequestInfo | URL, init?: DpopRequestInit): Promise<Response> => {
    const { mint, ...fetchInit } = init ?? {};
    // A Request of our own, so the caller's object is never modified. The
    // first attempt and the retry each send a clone of it. The origin check
    // reads the URL this Request resolved, never the input: a URL-like object
    // that is not a URL, or a relative URL under a <base href>, resolves only
    // here, and the check must see exactly what will be sent.
    const req = new Request(input, { ...fetchInit, redirect: "error" });
    const origin = new URL(req.url).origin;
    if (!allowed.has(origin)) {
      await req.body?.cancel().catch(() => undefined);
      throw new DpopOriginError(origin);
    }
    if (req.mode === "no-cors") {
      // no-cors silently drops non-safelisted headers: the proof would vanish.
      throw new Error("dpop: a no-cors request cannot carry a DPoP proof");
    }
    const token = (await opts.getToken?.()) || undefined;
    if (!token && mint !== true && !mintPaths.has(new URL(req.url).pathname)) {
      // No credential and no mint call: nothing to prove.
      return send(req);
    }
    const k = await key();
    const used = nonces.get(origin);

    let resp = await attempt(req, k, token, used);
    const fresh = learn(origin, resp);
    if (
      resp.status === 401 &&
      fresh &&
      fresh !== used &&
      isUseNonceChallenge(resp.headers.get("WWW-Authenticate"))
    ) {
      await resp.body?.cancel().catch(() => undefined);
      resp = await attempt(req, k, token, fresh);
      learn(origin, resp);
    }

    const reason = await refusalReason(resp);
    if (reason) throw new DpopRefusedError(reason, resp, k.thumbprint, discardKey);
    return resp;
  };

  return Object.assign(dpopFetch, { key, discardKey, reloadKey });
}
