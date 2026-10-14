// Token + identity helpers. The token lives in localStorage;
// httpOnly cookies need a CSRF strategy first.
//
// With device identity on (`auth.dpop`, see configureAuth below) the token
// is bound to this browser's device key and every request carries a DPoP
// proof instead of `Bearer`; the token may then also live in sessionStorage
// (a public computer) or in this tab's memory (no persistent key store).

import {
  createDpopFetch,
  defaultKeyStore,
  DpopRefusedError,
  isDeviceRevoked,
  type DpopFetch,
  type DpopRequestInit,
  type KeyStore,
} from "./dpop";
import type { AdminAuthSpec, AdminPageSpec } from "./types";

const TOKEN_KEY = "w17_admin_token";

// --- device identity state ---------------------------------------------
//
// All of it is inert until configureAuth turns device identity on: with
// `auth.dpop` absent or false, getToken/setToken/clearToken/authHeader are
// the localStorage + Bearer functions they always were.

let dpopOn = false;
// The signer every request goes through while device identity is on.
let signer: DpopFetch | null = null;
// True while a mint call (sign-in, OAuth exchange) is being handed to the
// signer: it must carry no token even when one is stored, or the edge would
// check that stale token before the sign-in route runs.
let minting = false;
// The token of this tab only, when the key store fell back to memory: after
// a reload this tab has a different key, so a token written to storage would
// be bound to a key no page holds any more.
let memoryToken: string | null = null;
let keyInMemory = false;
// The session lives in sessionStorage (a public computer): it follows no
// other tab and is gone when the browser closes.
let publicSession = false;
// Advanced by every sign-in, here or (by the storage event) in another tab.
// A DEVICE_REVOKED answer ends the session only when the refused request was
// sent under the current sign-in — never one that arrives after the user
// signed in again.
let signInGen = 0;
let storageListener: ((e: StorageEvent) => void) | null = null;
// The shared slot this tab last wrote its token to, and the token. Should
// the key store fall back to memory later, that token is bound to the key
// the fallback was seeded with — this tab's — so the tab goes on with it,
// and its sign-out still clears it.
let written: { slot: () => Storage | null; token: string } | null = null;
const revokedListeners = new Set<() => void>();

function localStore(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

function sessionStore(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

function read(store: Storage | null): string | null {
  try {
    return store?.getItem(TOKEN_KEY) ?? null;
  } catch {
    return null;
  }
}

function write(store: Storage | null, token: string | null): void {
  try {
    if (token === null) store?.removeItem(TOKEN_KEY);
    else store?.setItem(TOKEN_KEY, token);
  } catch {
    // Storage blocked (private mode / DOM sandbox): the SPA degrades to
    // in-memory only; the user signs in again on refresh.
  }
}

export function getToken(): string | null {
  if (dpopOn) {
    // A key in this tab's memory signs only this tab's token: the shared
    // slots hold tokens bound to the key other tabs sign with.
    if (keyInMemory) return memoryToken;
    // A public-computer session in THIS tab wins over the profile's.
    return read(sessionStore()) ?? read(localStore());
  }
  try {
    return window.localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

/** Options of {@link setToken}. */
export interface SetTokenOptions {
  /**
   * The sign-in response's `public_computer`: the token is kept in
   * sessionStorage only and the profile's localStorage token is removed.
   * Honoured only with device identity on.
   */
  publicComputer?: boolean;
}

export function setToken(token: string, opts?: SetTokenOptions): void {
  if (dpopOn) {
    signInGen++;
    publicSession = opts?.publicComputer === true;
    if (keyInMemory) {
      // Other tabs' tokens are theirs: neither read nor written from here.
      memoryToken = token;
    } else if (publicSession) {
      write(sessionStore(), token);
      write(localStore(), null);
      written = { slot: sessionStore, token };
    } else {
      write(localStore(), token);
      write(sessionStore(), null);
      written = { slot: localStore, token };
    }
    // Sign with the key the store holds, as the generated client does. It
    // differs from the key this sign-in was minted with only when another
    // tab replaced that key after a revocation — and a token bound to a
    // revoked key is dead either way.
    signer?.reloadKey();
    return;
  }
  try {
    window.localStorage.setItem(TOKEN_KEY, token);
  } catch {
    // localStorage blocked (private mode / DOM sandbox) — the
    // SPA degrades to in-memory only; user re-logs in on refresh.
  }
}

export function clearToken(): void {
  if (dpopOn) {
    memoryToken = null;
    publicSession = false;
    // A tab whose key is in memory clears only the token it wrote itself,
    // and only while the slot still holds it: anything else there is
    // another tab's, and clearing it would sign that tab out.
    if (keyInMemory) {
      if (written && read(written.slot()) === written.token) write(written.slot(), null);
      written = null;
      return;
    }
    written = null;
    write(localStore(), null);
    write(sessionStore(), null);
    return;
  }
  try {
    window.localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* ignore */
  }
}

// authHeader returns the Authorization header value to send on
// every API request, or undefined when the user hasn't logged
// in yet. With device identity on it is always undefined: the
// signer sets `Authorization: DPoP <token>` beside the proof, and
// the token must never travel as Bearer.
export function authHeader(): string | undefined {
  if (dpopOn) return undefined;
  const t = getToken();
  return t ? `Bearer ${t}` : undefined;
}

/** Test seams of {@link configureAuth}. */
export interface DeviceOptions {
  /** Where the device key lives. Default: the SDK's defaultKeyStore(). */
  keyStore?: KeyStore;
}

/**
 * Sets the SPA up for the admin it serves. Call once, before the first
 * request and before the redirect fragment is read.
 *
 * `auth.dpop` true turns device identity (DPoP, RFC 9449) on: every request
 * to the admin's own origin — and to `auth.oauth_exchange_endpoint`'s, when
 * that is another — is signed with this browser profile's device key, the
 * one IndexedDB holds for every tab. Absent or false, nothing changes: the
 * token is sent as Bearer, exactly as before.
 */
export function configureAuth(auth: AdminAuthSpec, opts: DeviceOptions = {}): void {
  if (storageListener) {
    window.removeEventListener("storage", storageListener);
    storageListener = null;
  }
  signer = null;
  written = null;
  memoryToken = null;
  keyInMemory = false;
  publicSession = false;
  minting = false;
  dpopOn = auth.dpop === true;
  if (!dpopOn) return;

  publicSession = read(sessionStore()) !== null;
  const page = pageBase();
  const origins = [new URL(page).origin];
  if (auth.oauth_exchange_endpoint) {
    const o = new URL(auth.oauth_exchange_endpoint, page).origin;
    if (!origins.includes(o)) origins.push(o);
  }
  // No IndexedDB at all: the key is a new one in every tab's memory, so
  // every token is this tab's alone from the start.
  if (!opts.keyStore && !(globalThis as { indexedDB?: IDBFactory }).indexedDB) keyInMemory = true;
  const keyStore =
    opts.keyStore ??
    defaultKeyStore(() => {
      // IndexedDB failed: the key is in this tab's memory from now on, and
      // so is the token of any later sign-in.
      keyInMemory = true;
      // The token this tab wrote, if the slot still holds it, is bound to
      // the key the fallback was seeded with: keep it rather than sign out.
      if (written && read(written.slot()) === written.token) memoryToken = written.token;
    });
  signer = createDpopFetch({
    origins,
    keyStore,
    getToken: () => (minting ? null : getToken()),
  });

  // Another tab signed in: it may have made a new key (after a revocation),
  // and the token in localStorage is bound to the key the store holds now.
  // A public-computer session, or one whose key is in this tab's memory
  // only, follows no other tab.
  storageListener = (e: StorageEvent) => {
    if (e.key !== TOKEN_KEY || e.storageArea !== localStore()) return;
    if (publicSession || keyInMemory) return;
    if (e.newValue !== null) signInGen++;
    signer?.reloadKey();
  };
  window.addEventListener("storage", storageListener);
}

/** True when configureAuth turned device identity on. */
export function deviceIdentity(): boolean {
  return dpopOn;
}

/** The document base relative endpoints resolve against. */
export function pageBase(): string {
  return document.baseURI;
}

/** Options of {@link deviceFetch}. */
export interface DeviceCallOptions {
  /**
   * A mint call (sign-in, OAuth exchange): it carries an unbound proof and no
   * token, even when one is stored.
   */
  mint?: boolean;
  /**
   * The mint may be sent twice (a password sign-in): when the stored key
   * turns out to be revoked, it is discarded and the call re-sent once with a
   * fresh key. A mint with one-time input (an OAuth code) is not.
   */
  resign?: boolean;
}

function mintCall(s: DpopFetch, url: string, init: DpopRequestInit): Promise<Response> {
  minting = true;
  try {
    // getToken is read synchronously, before the signer's first await.
    return s(url, { ...init, mint: true });
  } finally {
    minting = false;
  }
}

/**
 * Sends one request through the device signer. Only while device identity
 * is on (see {@link deviceIdentity}).
 *
 * A refusal comes back as the response it is (the caller reads its error
 * envelope), after the side effects it calls for: DEVICE_REVOKED discards the
 * key it was raised for and — when the refused request was sent under the
 * current sign-in — clears the session and tells {@link onDeviceRevoked}'s
 * listeners, once for any number of requests in flight.
 */
export async function deviceFetch(
  url: string,
  init: RequestInit,
  opts: DeviceCallOptions = {},
): Promise<Response> {
  const s = signer;
  if (!s) throw new Error("device identity is not configured");
  const sentGen = opts.mint ? null : signInGen;
  try {
    if (!opts.mint) return await s(url, init);
    try {
      return await mintCall(s, url, init);
    } catch (e) {
      if (!isDeviceRevoked(e) || !opts.resign) throw e;
      // The stored key was revoked: this sign-in makes a new device.
      try {
        await e.discardKey();
      } catch {
        // the retry still loads or makes a key
      }
      return await mintCall(s, url, init);
    }
  } catch (e) {
    if (isDeviceRevoked(e)) {
      await deviceRevoked(e, sentGen);
      return e.response;
    }
    if (e instanceof DpopRefusedError) return e.response;
    throw e;
  }
}

async function deviceRevoked(err: DpopRefusedError, sentGen: number | null): Promise<void> {
  try {
    await err.discardKey();
  } catch {
    // the next sign-in still asks for a fresh key
  }
  if (sentGen === null || sentGen !== signInGen || getToken() === null) return;
  clearToken();
  for (const l of revokedListeners) {
    try {
      l();
    } catch {
      // a listener must not mask the refusal
    }
  }
}

/**
 * Calls `listener` when the server refuses this device for good
 * (DEVICE_REVOKED) under the current session; the session is already
 * cleared and the key discarded. Returns the unsubscribe function.
 */
export function onDeviceRevoked(listener: () => void): () => void {
  revokedListeners.add(listener);
  return () => {
    revokedListeners.delete(listener);
  };
}

// hasAllPermissions reports whether the user's permission IDs
// are a superset of `required`. Both sides are the numeric
// lock-allocated IDs (whoami's `permission_ids[]` vs a page/
// action's `required_permissions[]`) — never perm-name
// strings. Empty / unset `required` returns true — endpoints
// with no perms gated land here. Empty / unset `userPerms`
// returns true only when `required` is also empty (a user with
// no perms can only see fully-open endpoints).
//
// Used by permission-aware hiding. The backend
// handler still enforces independently — this drives UX
// only; a malicious client that bypasses the SPA still hits
// the server's permission gate.
export function hasAllPermissions(
  userPerms: number[] | undefined | null,
  required: number[] | undefined | null,
): boolean {
  if (!required || required.length === 0) return true;
  if (!userPerms || userPerms.length === 0) return false;
  const have = new Set(userPerms);
  for (const r of required) {
    if (!have.has(r)) return false;
  }
  return true;
}

// pageVisibleTo reports whether a page should appear to the user
// in the nav AND on the overview tiles. A page is visible when
// either:
//   - it has no list view (overview-style / detail-only page —
//     always shown; reached via inlines or direct URL), OR
//   - whoami's permission_ids cover the list's required perms.
//
// Shared by App's nav filter and OverviewPage's default tiles so
// the two surfaces never disagree about what a user can see. The
// detail's own update/delete buttons gate themselves separately;
// the backend enforces independently (UX hint only).
export function pageVisibleTo(page: AdminPageSpec, userPerms: number[] | undefined): boolean {
  if (!page.list) return true;
  return hasAllPermissions(userPerms, page.list.required_permissions);
}

// --- federated sign-in: the token arrives in the URL fragment --------

// REDIRECT_PARAM is the fragment parameter a browser-redirect flow hands
// the session back under. Matches `browser_redirect.token_param`'s
// default on the gateway side; an endpoint that chose another name is
// read by passing it to consumeRedirectToken.
export const REDIRECT_PARAM = "token";

// consumeRedirectToken picks a session out of `location.hash`, stores
// it, and removes it from the URL. Returns true when one was found.
//
// # Why the fragment, and why it has to be consumed FIRST
//
// A federated sign-in ends with the identity provider sending the
// browser back here, and the gateway attaches the session as
// `#token=…`. A fragment never reaches a server — not this one, not a
// proxy, not an access log — which is why the credential travels there
// and not in the query.
//
// But this SPA also routes on the hash (`#/list/users`), so the two
// share one slot. The token has to be taken out before the router reads
// it, or the first render sees `#token=…`, matches no route, and the
// user lands on the default view with the address bar showing their
// session. Hence: called once, before the hash state is initialised.
//
// The hash is rewritten with `history.replaceState` rather than by
// assigning `location.hash`, because assigning pushes a new entry — the
// user's Back button would then walk them onto their own credential.
export function consumeRedirectToken(param: string = REDIRECT_PARAM): boolean {
  const token = takeFragmentParam(param);
  if (token === null) return false;
  setToken(token);
  return true;
}

/**
 * Takes `param` out of `location.hash` and returns its value, or null when
 * the fragment does not carry it (or carries it empty). The rest of the
 * fragment stays — a flow may carry its own routing state beside the
 * credential — and the URL is rewritten without a history entry, for the
 * reasons {@link consumeRedirectToken} gives.
 */
export function takeFragmentParam(param: string): string | null {
  let raw = "";
  try {
    raw = window.location.hash ?? "";
  } catch {
    return null;
  }
  if (raw === "" || raw === "#") return null;

  const params = new URLSearchParams(raw.replace(/^#/, ""));
  const value = params.get(param);
  if (value === null || value === "") return null;

  // Whatever else rode along stays — a flow may carry its own routing
  // state beside the credential. Only the credential is removed.
  params.delete(param);
  const rest = params.toString();
  const next = rest === "" ? "/" : "#" + rest;
  try {
    const url = window.location.pathname + window.location.search + (next === "/" ? "" : next);
    window.history.replaceState(null, "", url === "" ? "/" : url);
  } catch {
    // history unavailable (sandboxed frame): the value is taken, which
    // is the part that matters. The address bar keeps it, so degrade
    // rather than fail the sign-in.
  }
  return value;
}
