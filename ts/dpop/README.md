# @w17/dpop

The DPoP (RFC 9449) client the w17 browser clients sign their requests with.

- A device key made and kept by WebCrypto in IndexedDB; the private half never
  leaves the browser.
- Its RFC 7638 thumbprint, the device id the console shows.
- Proofs for a request, and a `fetch` wrapper that follows the server's nonces
  and types the `DEVICE_REVOKED` and `SESSION_EXPIRED` refusals.
- The stream-ticket helper for event streams.

It has no runtime dependencies. The Go verifier checks its proofs from a
committed fixture (`go/lib/dpop/testdata/ts_proofs.json`; `npm run fixture`
regenerates it). The admin runtime carries a copy of its browser sources
(`make sync-admin-dpop`).

`npm test` runs the tests (vitest).
