# wandering-compiler SDK

The **public, consumer-importable** surface of wandering-compiler — the runtime
bits a generated service or a hand-written business binary needs, with **no
dependency on the compiler internals**.

Everything the compiler *generates* runs against this SDK; the compiler itself
never leaks into it. That split is deliberate and load-bearing: a generated
project depends on this SDK and nothing else from the toolchain.

## Philosophy — dev-help, not a framework

Nothing in the SDK owns your control flow. A business layer is fully
implementable with plain `database/sql` and plain gRPC; you import an SDK package
only when it saves you boilerplate. Import à la carte, never wholesale.

## Languages

The SDK is organised **one directory per language**, each self-contained with its
own package manager and versioning:

| Language | Module / path | README |
|---|---|---|
| **Go** | `go/` (`github.com/wandering-compiler/sdk/go`) | [`go/README.md`](go/README.md) |
| **TypeScript** | `ts/admin-runtime/` (`@w17/admin-runtime`) | — |

The TypeScript side holds one package today: `@w17/admin-runtime`, the React
(Mantine) runtime behind a generated admin SPA. The scaffold the compiler emits
imports `bootstrap` from it and hands it the embedded admin spec; list, detail,
create, inline and overview pages are all rendered from that spec. Its tests run
with `npm test` (vitest) inside `ts/admin-runtime/`.

The generated front-end clients are emitted per project, not shipped here. A
further language gets its own top-level directory and README when it lands.

## Building and checking

- `make pb` regenerates `go/pb` from the protos in `go/proto` (protoc pinned in
  `tools/protoc`, run in Docker); `make check-pb` fails when the committed code
  differs.
- `scripts/check-leaks.sh [path…]` fails when a comment, test or file name
  carries a reference that only makes sense inside the project that develops
  the SDK, rather than the technical reason itself.

## What lives here vs. not

- **Here (public):** anything a consumer compiles against — a business handler, a
  plugin, a generated CLI-command body, a generated e2e test, the proto
  annotation vocabulary.
- **Not here:** library code only the code generator itself uses at build time —
  it is never shipped in this SDK, so importing it never drags in compiler
  internals.

See the per-language README for the concrete package map.
