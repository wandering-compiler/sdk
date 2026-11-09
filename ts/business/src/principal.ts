/**
 * The verified caller, read from a handler's context, and relayed onward.
 *
 * The TypeScript twin of `sdk/go/lib/principal`: the same metadata keys, the
 * same fail-closed readers, the same relay set and replace semantics. Any
 * change there is a change here (and a conformance case).
 *
 * The gateway stamps the principal as gRPC metadata on its first hop:
 *
 * - `x-w17-user` — base64 of the marshalled `AuthResp` (always present when
 *   authenticated);
 * - `x-w17-scope-<name>` — one per declared data scope;
 * - `w17-label-<name>` — broadcast labels;
 * - `x-w17-device`, `x-w17-dpop-jkt`, `x-w17-data-scope`, `x-w17-audience`,
 *   `x-w17-membership-kinds`, `x-w17-org-from`, `x-w17-host`.
 *
 * Trust boundary: the envelope is NOT signed. A tier believes it only because
 * the gateway strips these namespaces from client input and backend tiers are
 * reachable only from trusted tiers. Keep the business container off
 * untrusted networks.
 *
 * `AuthResp` is generated per project, so this module cannot name the type:
 * the readers take its schema, or use the one the SDK resolved at boot
 * (`setAuthRespType`: the descriptor set's single `AuthResp`).
 */

import { fromBinary, toBinary, type DescMessage, type Message } from "@bufbuild/protobuf";
import { Context, type Metadata } from "./context.js";
import { type AnyMessage, getDefaultPool } from "./descriptors.js";

export const USER_METADATA_KEY = "x-w17-user";
export const SCOPE_KEY_PREFIX = "x-w17-scope-";
export const LABEL_KEY_PREFIX = "w17-label-";
export const DEVICE_METADATA_KEY = "x-w17-device";
export const MINT_KEY_METADATA_KEY = "x-w17-dpop-jkt";
export const DATA_SCOPE_METADATA_KEY = "x-w17-data-scope";
export const AUDIENCE_KEY = "x-w17-audience";
export const MEMBERSHIP_KINDS_KEY = "x-w17-membership-kinds";
export const ORG_FROM_KEY = "x-w17-org-from";
export const HOST_KEY = "x-w17-host";

export const DATA_SCOPE_OWNER = "owner";
export const DATA_SCOPE_ORG = "org";
export const DATA_SCOPE_ALL = "all";

/** The exact relayed keys beside the two prefixes — principal.isRelayedKey. */
const RELAYED_EXACT = new Set([
  USER_METADATA_KEY,
  DATA_SCOPE_METADATA_KEY,
  DEVICE_METADATA_KEY,
  MINT_KEY_METADATA_KEY,
  AUDIENCE_KEY,
  MEMBERSHIP_KINDS_KEY,
  ORG_FROM_KEY,
  HOST_KEY,
]);

/** principal.gatewayOwnedPrefixes / gatewayOwnedKeys. */
const GATEWAY_OWNED_PREFIXES = ["x-w17-", LABEL_KEY_PREFIX];
const GATEWAY_OWNED_KEYS = new Set(["w17-tx-id", "w17-conn-id"]);

/**
 * No verified caller could be read from the context.
 *
 * Every failure mode collapses to this — absent metadata, an undecodable
 * envelope, an empty user id — because the distinction is never actionable
 * for the caller and leaking it tells an unauthenticated prober how far it
 * got. The cause stays on `cause` for a server-side log.
 */
export class NoPrincipalError extends Error {
  override name = "NoPrincipalError";
  constructor(detail = "", options?: ErrorOptions) {
    const msg = "principal: no authenticated caller in context";
    super(detail ? `${msg}: ${detail}` : msg, options);
  }
}

/** Go's sentinel name, for code that reads like the Go guide. */
export const ErrNoPrincipal = NoPrincipalError;

let authRespType: DescMessage | undefined;

/**
 * The project's AuthResp schema the readers decode into by default. The
 * runtime sets it at boot; tests may set it.
 */
export function setAuthRespType(schema: DescMessage | { schema: DescMessage } | undefined): void {
  authRespType = schema === undefined ? undefined : "schema" in schema ? schema.schema : schema;
}

function resolve(dst: DescMessage | { schema: DescMessage } | undefined): DescMessage {
  if (dst === undefined) {
    if (authRespType === undefined) throw new NoPrincipalError("no AuthResp type is known (pass its schema)");
    return authRespType;
  }
  return "schema" in dst ? dst.schema : dst;
}

/** `org_id` → `x-w17-scope-org_id`. */
export function scopeKey(name: string): string {
  return SCOPE_KEY_PREFIX + name;
}

/** `account_kind` → `w17-label-account_kind`. */
export function labelKey(name: string): string {
  return LABEL_KEY_PREFIX + name;
}

const B64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;

function envelopeFromMd(md: Metadata, schema: DescMessage): AnyMessage {
  const v = md.first(USER_METADATA_KEY);
  if (!v) throw new NoPrincipalError();
  // Go's base64.StdEncoding is strict; Buffer.from is not, so check first.
  if (!B64.test(v)) throw new NoPrincipalError("undecodable envelope");
  const raw = Buffer.from(v, "base64");
  try {
    return fromBinary(schema, raw) as AnyMessage;
  } catch (e) {
    throw new NoPrincipalError("unmarshal envelope", { cause: e });
  }
}

/**
 * The authenticated caller's user id; throws NoPrincipalError.
 *
 * Reads the cheap `x-w17-scope-user_id` first, then decodes the envelope —
 * the fallback is what makes it work in a project scoped by something other
 * than user_id, where the gateway threads no user_id scope.
 */
export function userId(ctx: Context, dst?: DescMessage | { schema: DescMessage }): string {
  const v = ctx.incoming.first(scopeKey("user_id"));
  if (v) return v;
  const msg = envelopeFromMd(ctx.incoming, resolve(dst));
  const uid = (msg as { userId?: unknown }).userId;
  if (typeof uid === "string" && uid) return uid;
  throw new NoPrincipalError();
}

/** Decode the full verified principal (the project's AuthResp). */
export function envelope(ctx: Context, dst?: DescMessage | { schema: DescMessage }): AnyMessage {
  return envelopeFromMd(ctx.incoming, resolve(dst));
}

/**
 * A declared data scope's value, undefined when absent. Fail closed on
 * undefined — an absent scope is never "match everything".
 */
export function scope(ctx: Context, name: string): string | undefined {
  return ctx.incoming.first(scopeKey(name)) || undefined;
}

/**
 * A broadcast label, undefined when absent. Absence must deny: a rule reads
 * "refuse unless the label says X".
 */
export function label(ctx: Context, name: string): string | undefined {
  return ctx.incoming.first(labelKey(name)) || undefined;
}

/**
 * The verified device id (DPoP key thumbprint), undefined when absent.
 * Exactly one non-empty value counts, as in Go.
 */
export function deviceId(ctx: Context): string | undefined {
  const vals = ctx.incoming.getAll(DEVICE_METADATA_KEY);
  if (vals.length !== 1 || !vals[0]) return undefined;
  return vals[0];
}

/**
 * The audience data range: "owner", "org", "all", or "" when none.
 * An unknown value, or several, reads as the narrowest range ("owner").
 */
export function dataScope(ctx: Context): string {
  const vals = ctx.incoming.getAll(DATA_SCOPE_METADATA_KEY);
  if (!vals.length) return "";
  if (vals.length === 1 && [DATA_SCOPE_OWNER, DATA_SCOPE_ORG, DATA_SCOPE_ALL].includes(vals[0]!)) return vals[0]!;
  return DATA_SCOPE_OWNER;
}

/** Whether a (lower-case) key is one `forwardToOutgoing` relays. */
export function isRelayedKey(key: string): boolean {
  return RELAYED_EXACT.has(key) || key.startsWith(SCOPE_KEY_PREFIX) || key.startsWith(LABEL_KEY_PREFIX);
}

/**
 * principal.IsGatewayOwnedKey: `x-w17-*`, `w17-label-*`, and the exact
 * `w17-tx-id` / `w17-conn-id`. Case-insensitive.
 */
export function isGatewayOwnedKey(key: string): boolean {
  const k = key.trim().toLowerCase();
  return GATEWAY_OWNED_PREFIXES.some((p) => k.startsWith(p)) || GATEWAY_OWNED_KEYS.has(k);
}

/**
 * Relay the verified principal from INCOMING onto OUTGOING metadata.
 *
 * Moves exactly the gateway stamps (see `isRelayedKey`) and nothing else:
 * paging, tracing, locale and transaction routing have their own propagation.
 *
 * The verified principal wins: when the incoming side carries any relayed
 * key, the outgoing side's whole relayed set is REPLACED — an outgoing value
 * for a relayed key is overwritten, and an outgoing-only relayed key is
 * dropped, because nothing distinguishes a deliberate override from a context
 * reused across requests, and the second would scope the next hop to someone
 * else's rows. Every other outgoing key is kept. With no incoming principal
 * the context is returned unchanged. Idempotent.
 *
 * Every storage client call applies it, as Go's `grpcx.DialOpts` does.
 */
export function forwardToOutgoing(ctx: Context): Context {
  const relayed = new Map<string, readonly string[]>();
  for (const [k, v] of ctx.incoming) if (isRelayedKey(k) && v.length) relayed.set(k, v);
  if (!relayed.size) return ctx;
  return ctx.withOutgoing(ctx.outgoing.without(isRelayedKey).merged(relayed));
}

/**
 * A context that calls a tier of THIS domain as `user`.
 *
 * The envelope (the project's AuthResp; its user id is required) and the
 * named scopes are stamped on the OUTGOING side in place of any principal ctx
 * carries, and every relayed key is dropped from the INCOMING side too, so a
 * later `forwardToOutgoing` cannot put a different principal back.
 *
 * Throws TypeError for a missing user id or a malformed scope.
 */
export function actAs(ctx: Context, user: Message, scopes: Readonly<Record<string, string>> = {}): Context {
  const uid = (user as { userId?: unknown } | undefined)?.userId;
  if (typeof uid !== "string" || !uid) throw new TypeError("principal: actAs needs a user id");
  const schema = getDefaultPool().schema(user.$typeName);
  const stamps = new Map<string, readonly string[]>([
    [USER_METADATA_KEY, [Buffer.from(toBinary(schema, user)).toString("base64")]],
  ]);
  for (const [name, v] of Object.entries(scopes)) {
    if (!name || !v || name.trim() !== name) {
      throw new TypeError(`principal: actAs: scope ${JSON.stringify(name)}=${JSON.stringify(v)} is not a scope`);
    }
    stamps.set(scopeKey(name), [v]);
  }
  const stripped = ctx.withIncoming(ctx.incoming.without(isRelayedKey));
  return stripped.withOutgoing(stripped.outgoing.without(isRelayedKey).merged(stamps));
}
