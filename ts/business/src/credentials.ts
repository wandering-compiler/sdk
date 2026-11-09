/**
 * Which metadata keys carry a request credential (eventbus.isCredentialKey):
 * stripped from an event's envelope, and scrubbed from a Sentry event.
 */

import { isGatewayOwnedKey } from "./principal.js";

const CREDENTIAL_HEADERS = new Set(["authorization", "proxy-authorization", "cookie", "set-cookie", "x-w17-credential"]);
const CREDENTIAL_SEGMENTS = new Set([
  "auth", "authorization", "apikey", "cookie", "credential", "credentials",
  "jwt", "passwd", "password", "secret", "session", "token",
]);
const GRPC_GATEWAY_PREFIX = "grpcgateway-";
const TX_KEYS = new Set(["w17-tx-id", "w17-conn-id"]);

/** Whether a metadata key carries a request credential (eventbus.isCredentialKey). */
export function isCredentialKey(key: string): boolean {
  let k = key.trim().toLowerCase();
  if (k.startsWith(GRPC_GATEWAY_PREFIX)) k = k.slice(GRPC_GATEWAY_PREFIX.length);
  if (CREDENTIAL_HEADERS.has(k)) return true;
  if (isGatewayOwnedKey(k)) return false;
  const segs = k.split(/[-_.]/).filter(Boolean);
  for (let i = 0; i < segs.length; i++) {
    if (CREDENTIAL_SEGMENTS.has(segs[i]!)) return true;
    if (segs[i] === "api" && segs[i + 1] === "key") return true;
  }
  return false;
}

/** Stripped from an envelope: the transaction keys and credentials. */
export function reservedMetadataKey(key: string): boolean {
  return TX_KEYS.has(key.toLowerCase()) || isCredentialKey(key);
}
