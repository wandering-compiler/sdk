# Behaviour specification

This directory specifies what the Go SDK promises: for every feature, what it is for,
how it is configured, what it MUST guarantee, what MUST be impossible, and which test
proves each guarantee. It is a contract for users of the SDK and a checklist for anyone
changing it.

| File | Area |
|---|---|
| [server.md](server.md) | Process startup and lifecycle: supervisor, gRPC server, bounded stops, health, internal-mesh TLS, observability, in-process bridge |
| [edges.md](edges.md) | Edges and traffic: REST gateway, authentication, streams, uploads, rate limiting, MCP, client dial, error mapping |
| [data.md](data.md) | Data layer: database sessions, transactions, SQL helpers, paging, blob and key-value stores, field encryption, secrets |
| [events-identity.md](events-identity.md) | Events, identity and language: event bus, principal, permission checks, i18n, small helpers |
| [tooling.md](tooling.md) | Tooling: migrations, data migrations, the e2e runner, dev PKI, digests |
| [matrix.md](matrix.md) | Feature-interaction matrix across all areas |

## Totals

| Area | Features | Guarantees | No test | Covered in part |
|---|---|---|---|---|
| [server.md](server.md) | 21 | 164 | 0 | 8 |
| [edges.md](edges.md) | 34 | 209 | 2 | 4 |
| [data.md](data.md) | 17 | 134 | 2 | 2 |
| [events-identity.md](events-identity.md) | 18 | 114 | 0 | 1 |
| [tooling.md](tooling.md) | 29 | 170 | 0 | 0 |
| **All areas** | **119** | **791** | **4** | **15** |
| [matrix.md](matrix.md) | — | 129 interactions | 20 | 32 |

A row is "covered in part" when its test is followed by a note that limits it, or when part of
it says **no test**.

## Method

Each feature section answers the same questions:

1. **Purpose** — what the feature is for.
2. **Configuration** — every setting, its default, and what happens with invalid or edge values.
3. **Shares** — code or state it shares with other features (globals, pools, contexts, environment variables).
4. **Guarantees** — what it MUST do and what MUST be impossible. MUST NOT rows cover security,
   concurrency, performance and resource limits, and behaviour on failure.
5. **Deliberate limits** — what it intentionally does not do.
6. **Test** — for every guarantee, the test that proves it.

Guarantees are numbered per area: `S` server, `E` edges, `D` data, `I` events and identity,
`T` tooling, then the feature and the guarantee (`S12.3` is the third guarantee of feature
S12). Code is cited by path from the repository root and symbol name, not by line number.

A guarantee without a test says **no test** in its Test column. That is a statement about
coverage, not about whether the code behaves correctly. The goal is that every row names a
test. "Holds by construction" means the property follows from the shape of the code; it still
counts as untested until a test pins it.

## Interactions

Most defects live where two features meet: a cache in front of an authorization check, a
retry around a non-idempotent write, a shutdown hook that closes a pool a handler still uses.
[matrix.md](matrix.md) lists every pair of features whose combination changes behaviour, the
expected behaviour of the combination, and the test that proves it. A pair is listed once,
under the pair of areas it spans.

## Using it

- **Changing a feature:** update its section in the same change — configuration, guarantees,
  limits — and the matrix rows that name it.
- **Adding a guarantee:** add the row together with the test that proves it. Name the test in
  the Test column.
- **Adding a feature:** add a section with all six parts, and a matrix row for every existing
  feature whose behaviour it changes or depends on.
- **Reviewing:** a change that alters behaviour without touching this directory, or a row whose
  named test no longer exists, is incomplete.
- **Closing gaps:** pick a **no test** row, write the test, and replace the marker with its name.
