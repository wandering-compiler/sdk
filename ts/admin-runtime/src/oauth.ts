// Federated sign-in: the PKCE code flow of the auth plugin's OAuth feature.
//
//   1. A sign-in button makes a PKCE verifier, keeps it in this tab's
//      sessionStorage and starts the flow at its `start_url` with the
//      verifier's S256 challenge added as `code_challenge`.
//   2. The identity provider sends the browser to the plugin's callback,
//      which redirects back here with a single-use login code in the
//      fragment: `#code=…` (`browser_redirect.token_param: code`).
//   3. The SPA redeems the code at `auth.oauth_exchange_endpoint` with the
//      verifier — a mint call, so with device identity on it carries an
//      unbound proof and the session is bound to this browser's key.
//
// The callback never hands out a session: a URL can be handed to anyone, and
// only the browser holding the verifier can redeem the code.
//
// An admin whose spec has no `oauth_exchange_endpoint` keeps the older shape:
// plain links, and a session in `#token=…` (Bearer only — a token in a URL
// was never bound to any key, so a device-binding admin refuses it).

import { apiPost } from "./api";
import { REDIRECT_PARAM, deviceIdentity, setToken, takeFragmentParam } from "./auth";
import { base64urlEncode } from "./dpop";
import type { AdminAuthSpec } from "./types";

/** The fragment parameter the plugin's callback carries the login code in. */
export const CODE_PARAM = "code";

// Where the verifier waits for the browser to come back: this tab only, and
// gone with it.
const PENDING_KEY = "w17_admin_oauth";

interface Pending {
  verifier: string;
  public_computer?: boolean;
}

/** What {@link consumeRedirect} found in the fragment. */
export type Redirect =
  | { kind: "none" }
  /** A session in `#token=`, stored (Bearer admins without an exchange endpoint). */
  | { kind: "token" }
  /** A session in `#token=` that this admin cannot use; it was dropped. */
  | { kind: "token-refused" }
  /** A login code to redeem with {@link exchangeCode}. */
  | { kind: "code"; code: string };

/**
 * Takes a federated sign-in's result out of the URL fragment, before the
 * router reads it (see auth.ts::consumeRedirectToken for why first, and why
 * without a history entry).
 *
 * With an exchange endpoint the fragment's `code` is taken for redemption. A
 * `token` is stored only on an admin without device identity: with it on, a
 * token that travelled in a URL is bound to no key and would be refused, so
 * it is removed from the URL and dropped, and the caller asks the user to
 * sign in again.
 */
export function consumeRedirect(auth: AdminAuthSpec): Redirect {
  if (auth.oauth_exchange_endpoint) {
    const code = takeFragmentParam(CODE_PARAM);
    if (code !== null) return { kind: "code", code };
  }
  const token = takeFragmentParam(REDIRECT_PARAM);
  if (token === null) return { kind: "none" };
  if (deviceIdentity()) return { kind: "token-refused" };
  setToken(token);
  return { kind: "token" };
}

function session(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

/** A PKCE verifier (RFC 7636 §4.1): 32 random bytes, base64url — 43 characters. */
export function newVerifier(): string {
  return base64urlEncode(crypto.getRandomValues(new Uint8Array(32)));
}

/** The S256 challenge of a verifier (RFC 7636 §4.2). */
export async function challengeOf(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return base64urlEncode(new Uint8Array(digest));
}

/**
 * The URL a sign-in button sends the browser to: `startUrl` with a new PKCE
 * challenge added, its verifier (and the public-computer choice, which the
 * exchange carries) kept in this tab's sessionStorage.
 */
export async function federatedStartURL(
  startUrl: string,
  opts: { publicComputer?: boolean } = {},
): Promise<string> {
  const verifier = newVerifier();
  const pending: Pending = { verifier };
  if (opts.publicComputer) pending.public_computer = true;
  try {
    session()?.setItem(PENDING_KEY, JSON.stringify(pending));
  } catch {
    // Without storage the code cannot be redeemed when the browser comes
    // back; the exchange then says so instead of failing here.
  }
  const url = new URL(startUrl, document.baseURI);
  url.searchParams.set("code_challenge", await challengeOf(verifier));
  return url.href;
}

function takePending(): Pending | null {
  const s = session();
  let raw: string | null = null;
  try {
    raw = s?.getItem(PENDING_KEY) ?? null;
    // Single use, like the code it redeems.
    s?.removeItem(PENDING_KEY);
  } catch {
    return null;
  }
  if (raw === null) return null;
  try {
    const p = JSON.parse(raw) as Partial<Pending> | null;
    return typeof p?.verifier === "string" && p.verifier !== ""
      ? { verifier: p.verifier, public_computer: p.public_computer === true }
      : null;
  } catch {
    return null;
  }
}

interface ExchangeResp {
  token?: string;
  public_computer?: boolean;
}

/** A sign-in that cannot be redeemed in this tab. */
export class FederatedSignInError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "FederatedSignInError";
  }
}

/**
 * Redeems a login code at `auth.oauth_exchange_endpoint` with the verifier
 * this tab kept, and stores the session it returns. A mint call: with device
 * identity on it carries an unbound proof and no token, and it is never
 * re-sent — the code is single-use.
 *
 * Throws {@link FederatedSignInError} when this tab holds no verifier (the
 * flow was started in another tab, or storage is blocked), and the API's
 * error when the exchange is refused.
 */
export async function exchangeCode(auth: AdminAuthSpec, code: string): Promise<void> {
  const endpoint = auth.oauth_exchange_endpoint;
  if (!endpoint) throw new FederatedSignInError("this admin has no sign-in code exchange");
  const pending = takePending();
  if (!pending) {
    throw new FederatedSignInError(
      "the sign-in was started in another tab or window; start it again here",
    );
  }
  const body: Record<string, unknown> = { code, code_verifier: pending.verifier };
  if (pending.public_computer) body.public_computer = true;
  const resp = await apiPost<ExchangeResp>(endpoint, body, undefined, { mint: true });
  if (!resp?.token) throw new FederatedSignInError("sign-in response missing 'token' field");
  // The choice made at the start counts even when the answer cannot echo it.
  setToken(resp.token, {
    publicComputer: resp.public_computer === true || pending.public_computer === true,
  });
}
