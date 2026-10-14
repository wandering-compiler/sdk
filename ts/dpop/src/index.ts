// @w17/dpop — the DPoP client (RFC 9449) for the platform's TypeScript
// clients. Zero runtime dependencies: signing and hashing are WebCrypto
// (globalThis.crypto.subtle, browsers and Node >= 20); this package only
// assembles the JWS and JWK JSON. The Node-only file key store is in
// "@w17/dpop/node" so that nothing here imports a Node module.

export { base64urlDecode, base64urlEncode } from "./base64url";
export { canonicalJwk, jwkThumbprint, publicJwk, type PublicJwk } from "./jwk";
export {
  defaultKeyStore,
  dpopKey,
  FallbackKeyStore,
  generateKeyPair,
  IndexedDBKeyStore,
  loadOrCreateKey,
  MemoryKeyStore,
  type DpopKey,
  type IndexedDBKeyStoreOptions,
  type KeyStore,
} from "./key";
export {
  accessTokenHash,
  createProof,
  newJTI,
  normalizeHTU,
  PROOF_TYPE,
  signClaims,
  type ProofClaims,
  type ProofHeader,
  type ProofParams,
} from "./proof";
export {
  AUTH_SCHEME,
  createDpopFetch,
  DpopOriginError,
  DpopRefusedError,
  HEADER_NONCE,
  HEADER_PROOF,
  isDeviceRevoked,
  isSessionExpired,
  isUseNonceChallenge,
  MAX_REFUSAL_BODY,
  type DpopFetch,
  type DpopFetchOptions,
  type DpopRequestInit,
  type RefusalReason,
  type TokenSource,
} from "./fetch";
export {
  DEFAULT_TICKET_PATH,
  DpopTicketError,
  issueStreamTicket,
  withTicket,
  type StreamTicket,
  type StreamTicketOptions,
} from "./ticket";
