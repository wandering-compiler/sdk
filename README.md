# wandering-compiler SDK

The **public, consumer-importable** surface of wandering-compiler — the runtime
bits a generated service or a hand-written business binary needs, with **no
dependency on the compiler internals**.

Everything the compiler *generates* runs against this SDK; the compiler itself
never leaks into it. That split is deliberate and load-bearing: the SDK ships to
public repos, the compiler stays private.

## Philosophy — dev-help, not a framework

Nothing in the SDK owns your control flow. A business layer is fully
implementable with plain `database/sql` and plain gRPC; you import an SDK package
only when it saves you boilerplate. Import à la carte, never wholesale.

## Languages

The SDK is organised **one directory per language**, each self-contained with its
own package manager, versioning, and README:

| Language | Module / path | README |
|---|---|---|
| **Go** | `sdk/go` (`github.com/wandering-compiler/sdk/go`) | [`go/README.md`](go/README.md) |

Only Go exists today. Additional client languages (the generated FE clients are
emitted per project, not shipped here) get their own `sdk/<lang>/` directory + a
language-specific README when they land.

## What lives here vs. not

- **Here (public):** anything a consumer compiles against — a business handler, a
  plugin, a generated CLI-command body, a generated e2e test, the proto
  annotation vocabulary.
- **Not here:** library code only the code generator itself uses at build time —
  it is never shipped in this SDK, so importing it never drags in compiler
  internals.

See the per-language README for the concrete package map.
