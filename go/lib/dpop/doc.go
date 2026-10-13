// Package dpop implements Demonstrating Proof of Possession (RFC 9449) for
// device-bound credentials: the device key, its RFC 7638 thumbprint (the
// device id), the proof a client attaches to every request, and the
// server-side verifier.
//
// # Wire
//
// The package follows RFC 9449 as written:
//
//   - `Authorization: DPoP <access token>` carries the token, `DPoP: <proof>`
//     the proof — exactly one DPoP header per request.
//   - The proof is a compact JWS with `typ: dpop+jwt`, `alg` ES256 or EdDSA
//     (every other algorithm, `none` and the symmetric ones included, is
//     refused) and the public device key in `jwk`, which must hold no private
//     members.
//   - Claims: `jti` (unique per proof), `htm` (the HTTP method), `htu` (the
//     target URI without query or fragment), `iat`, `ath` (base64url SHA-256
//     of the access token; absent when the request carries no token) and
//     `nonce` (the last server-provided nonce).
//   - A server that wants a fresh nonce answers 401 with
//     `WWW-Authenticate: DPoP error="use_dpop_nonce"` and the nonce in
//     `DPoP-Nonce`; the client retries once with that nonce.
//
// gRPC carries the same values as metadata: `authorization` and `dpop` on the
// request; `dpop-nonce` in response headers or trailers; a refusal is
// codes.Unauthenticated with the challenge in a `www-authenticate` trailer
// ([ChallengeMetadata]). The htm of a gRPC call is POST and its htu is the
// configured public origin followed by the full method name,
// `<origin>/<package.Service>/<Method>`.
//
// # Verifier
//
// [Verifier.Verify] runs the checks in this order: proof header (count,
// size, alg, typ, jwk), signature, nonce (or iat when nonces are off), htm
// and htu, ath, the key thumbprint against the binding the caller expects,
// and last the jti. Each cause has its own error ([ErrMissingProof],
// [ErrBadAlg], …); a stale or missing nonce is a [*NonceError] carrying the
// fresh nonce the caller must send back with `use_dpop_nonce`.
//
// The header and the claims are decoded strictly: member names are matched
// exactly (case-sensitive), a duplicate member is malformed, and nothing may
// follow the JSON object.
//
// The htu is compared against [Config.Origin] plus the request path, never
// against the Host header, so a proof cannot be redirected by a client that
// controls Host.
//
// Nonces are stateless ([HMACNonces]): an HMAC over a time bucket and the key
// thumbprint, with a key shared by every replica, so a nonce one replica
// issued is accepted by all of them. The current and the previous bucket are
// valid.
//
// Replay protection records the jti of every accepted proof in a [JTIStore],
// and only of accepted ones: the insert is the last step, so a proof refused
// for any reason takes no room. The store bounds its entries in total and
// per key thumbprint. A mint endpoint (no access token, no binding) accepts
// a proof from any key, so anyone can mint keys and fill the store there:
// mint endpoints need rate limiting in front of the verifier. Unbound
// proofs are recorded in a store of their own ([Config.MintJTIs]), so such a
// flood can stall minting but never authenticated requests; with
// [Config.MintFull] set to [MintFullAcceptUnrecorded] it does not even stall
// minting, at the cost of best-effort replay protection for unbound proofs.
// A full or failing store is [ErrUnavailable], answered 503 ([HTTPStatus]). The
// in-memory [MemoryJTIStore] is per process and is correct only for a single
// replica: with two or more, a proof accepted by one replica can be replayed
// against another. A multi-replica deployment needs a shared store.
//
// A proof may be presented again, byte for byte, while its reuse window
// ([Config.ReuseWindow], default 30 s) lasts — a client retrying the same
// request does not need a new proof. The nonce and jti checks run on every
// presentation; only the signature verification result is cached, keyed by
// the SHA-256 of the proof.
package dpop
