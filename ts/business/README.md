# w17 business SDK for TypeScript

`@w17/business` serves a domain's business tier written in TypeScript. Codegen
writes no TypeScript for it: it writes `descriptors.binpb` (the domain's
protos, compiled) and `w17-business.json` (the manifest: config keys, storage
services, events). The SDK builds the services from them at boot and applies
what sdk/go's generated wrappers do as middleware: header forwarding,
distributed transactions, events after commit, `w17.ErrorDetail` errors.

Node 24, ESM, on `@grpc/grpc-js` and protobuf-es v2 (`@bufbuild/protobuf`).
Your code is TypeScript run by Node's built-in type stripping: there is no
build step.

```
node node_modules/@w17/business/dist/main.js serve --manifest w17-business.json --project-root .
node node_modules/@w17/business/dist/main.js health --manifest w17-business.json   # container HEALTHCHECK
```

The package also installs the bin `w17-business` (same commands).

## Install

There is no package registry. The image fetches this repository at the tag
`ts/<sdk_version>` (or the commit of a pseudo-version), builds the package
from source, packs it and installs the tarball:

```
cd ts/business && npm ci && npm run build && npm pack
npm install --prefix /app --omit=dev ./w17-business-<version>.tgz
node /app/node_modules/@w17/business/dist/main.js serve ...
```

When a project is generated against a checkout (co-development), the image
gets this directory as the named build context `w17-sdk-ts`
(`<checkout>/sdk/ts/business`) instead of the tag.

## What you write

The manifest's `register` names your package's directory (`path`, relative to
the project root) and its entry module (`module`, `w17.ts`), which exports one
function. Your handler classes name their methods **exactly** as the RPCs:

```ts
// <register.path>/w17.ts
import { distx, onCommit, principal } from "@w17/business";
import type { AnyMessage, Clients, Config, Context, MessageType, Registry, ServiceClient, TxCoordinator } from "@w17/business";
import { sendMail } from "./mail.ts";

class CheckoutService {
  orders: ServiceClient;
  tx: TxCoordinator;
  registry: Registry;
  M: (name: string) => MessageType;

  constructor(cfg: Config, registry: Registry, clients: Clients) {
    this.orders = clients.service("shop.core.OrderMutation");
    this.tx = clients.distributedTransaction();
    this.registry = registry; // compose through registry.self(...)
    this.M = (name) => registry.pool.message(name);
  }

  async PlaceOrder(ctx: Context, req: AnyMessage): Promise<AnyMessage> {
    const uid = principal.userId(ctx);
    return distx.run(ctx, this.tx, distx.beginRequest("shop-postgres"), async (txCtx) => { // the connection, by its lock name
      const order = await this.orders.CreateOrder(txCtx, this.M("shop.core.CreateOrderReq")({ userId: uid }));
      await onCommit(txCtx, () => sendMail(order)); // never for a rollback
      return this.M("shop.core.PlaceOrderResp")({ orderId: order.id });
    });
  }
}

export function registerBusiness(cfg: Config, registry: Registry, clients: Clients): void {
  registry.register("shop.core.CheckoutService", new CheckoutService(cfg, registry, clients));
}
```

- **Erasable TypeScript only.** Node strips the types and runs what is left,
  so syntax that needs compiling is refused: no `enum`, no `namespace`, no
  constructor parameter properties (`constructor(readonly x)`). Write relative
  imports with their `.ts` extension (`./mail.ts`) and type-only imports with
  `import type`. In your `tsconfig.json` (for the editor and `tsc --noEmit`):
  `"erasableSyntaxOnly": true`, `"verbatimModuleSyntax": true`,
  `"allowImportingTsExtensions": true`, `"module": "NodeNext"`, `"noEmit": true`.
- **Your `package.json`**: `"type": "module"` (or Node warns and reparses
  every file). Leave `@w17/business` out of it: at run time
  `import ... from "@w17/business"` resolves to the copy the image installed
  in `/app/node_modules`, the same instance as the runner. For the editor's
  types, list it only as a `file:` link to a local checkout,
  `"devDependencies": { "@w17/business": "file:../path/to/sdk/ts/business" }`.
  **A registry version spec (`"@w17/business": "0.1.0"`, `"^0.1.0"`) breaks
  the image build — under `devDependencies` too:** there is no package in the
  npm registry, and npm resolves a dev dependency's metadata even when it
  installs with `--omit=dev` (without a lockfile it fails with E404).
- **Messages are protobuf-es v2 messages** built from the descriptor set:
  `registry.pool.message("pkg.Name")({ ... })` creates one; fields are the
  camelCase `localName` (`user_id` → `userId`), 64-bit integers are `bigint`,
  a oneof is `{ case, value }`, a map a plain object.
- **The context is explicit.** Every handler gets `ctx` first and passes it to
  every call. It carries the incoming metadata, the deadline and the open
  transaction. Helpers return a new context (`paging.forward`,
  `principal.actAs`, `distx.begin`); none changes the one it was given. There
  is no `AsyncLocalStorage`. `ctx.signal` is an `AbortSignal` for APIs that
  take one (`fetch`).
- **Compose through `registry.self(Service)`.** A direct call on your class
  skips the method's event, and nothing fails — the Go rule. As in Go it is a
  direct call: the callee gets your ctx unchanged (incoming metadata, the open
  transaction it joins, whatever you put on it). Pass a `paging.forward(ctx)`
  only to the storage call that returns the page, never to `self`; and
  `actAs` on a ctx passed to `self` is not seen by the callee's principal
  readers (it sets the outgoing side), as in Go.
- **Work after a commit outlives the request.** Events and `onCommit` hooks of
  a committed transaction run to their end even when the client hangs up; the
  drain waits for them.
- **Streams:** a server-streaming handler is an async generator
  (`async *Watch(ctx, req)`); a client-streaming one takes an async iterable
  of requests; bidi both. A caller that hangs up, or whose deadline passes,
  mid-stream makes that iterable throw a `StatusError` (`CANCELLED`,
  `DEADLINE_EXCEEDED`) as Go's `Recv` returns one; rethrown, it answers as it
  is, never as an internal error. A handler may stop reading early (`break`
  out of its `for await`, or answer a client stream after the first request):
  that only stops reading, as in Go, and the replies and the status are still
  sent; what the caller sends after that is dropped.
- **Calling a stream:** a client-streaming or bidi call takes your iterable of
  requests and stops pulling from it once the call ends — the server answered
  or failed first — and closes it (`return()`, so a generator's `finally`
  runs).
- The boot refuses a handler that lacks a method of its service (or whose
  method is not `async`, or not `async *` for a server-streaming one), a
  service the manifest does not list, and a plugin hook bound to a method with
  no handler.

## API

| | |
|---|---|
| `Context`, `Metadata`, `background()` | the immutable request context; durations in milliseconds |
| `principal.userId / envelope / scope / label / deviceId / dataScope` | the verified caller, fail-closed (`NoPrincipalError`) |
| `principal.forwardToOutgoing(ctx)`, `principal.actAs(ctx, user, scopes)` | the relay (every storage call applies it) and acting as someone |
| `paging.forward(ctx)` | the incoming page, onto the calls you pass it to only; the page position the call answers with (`x-w17-paging-next` / `-prev` / `-total`) comes back as your method's response headers, so the gateway builds the next and previous cursor without your code knowing the sort |
| `distx.run(ctx, client, req, fn)`, `distx.begin(ctx, client, req)` → `TxHandle.commit/rollback` | transactions; nested ones join |
| `distx.RollbackOnlyError`, `NestedMismatchError`, `HandleFinishedError`, `CommitOutcomeUnknownError` | Go's four transaction errors |
| `onCommit(ctx, fn)` | run after the root commit, never on rollback, at once outside a transaction |
| `forUser(code, detailCode, message, field = "", dev?)` | a status with a `w17.ErrorDetail` (`Status.FAILED_PRECONDITION`, …) |
| `Config.str / int / bigint / bool / secret` | the manifest's keys; `Secret` prints `***` |
| `Clients.service(name)`, `Clients.distributedTransaction()` | storage clients (retry on UNAVAILABLE, keepalive, internal TLS) |
| `Registry.register / self / registerShutdown`, `Registry.pool.message(name)` | serving, composing, drain hooks, message types |

Errors a handler throws: a `StatusError` (`forUser`'s, or one a storage call
threw) goes out as it is, details included; an SDK error (`DistxError`,
`NoPrincipalError`, `ConfigError`) answers `UNKNOWN` with its message;
anything else is reported and answers `INTERNAL` `"<method>: internal error"`.

Environment read by the runtime itself, as the Go runtime reads it:
`W17_INTERNAL_TLS` (`_CERT`, `_KEY`, `_CA`, `_CLIENT_CA`), `W17_SENTRY_DSN`,
`W17_OTEL_ENDPOINT`, `W17_OTEL_TLS`, `W17_ENV`, `W17_SERVICE_VERSION`,
`W17_SECRETS_MODE`, `W17_SECRETS_AGE_KEY(_FILE)`, and
`W17_BUSINESS_MANIFEST` / `W17_BUSINESS_PROJECT_ROOT` for the flags' defaults.

Where JavaScript differs from Go and Python: a running promise cannot be
cancelled. A cancelled context cancels the calls made with it and reads as
done, but your handler runs on until it returns. The drain waits for it — the
server's close cancels what is still running after the shutdown timeout, then
gives those handlers, and the work still owed after a commit, one more
timeout together (not one each) to finish (a rollback, a commit, its events)
before the storage channels and the buses close; when that passes, they are
abandoned (and reported), not interrupted. The whole drain — the server's
stop, those waits, the shutdown hooks and the bus close — draws from one
budget set when it starts: 30 s (Go's drain timeout), or the shutdown timeout
if that is longer; the telemetry flush (Sentry, at most 5 s) comes after it.
Give the container a termination grace longer than that.
`cfg.int(name)` is a `number` and refuses a value beyond 2^53;
`cfg.bigint(name)` reads any 64-bit value.

## Tests

- `make test-ts-business` — typecheck (the SDK, and the conformance service
  under an author's `tsconfig`), lint, and the unit tests (vitest), in Docker.
- `make conformance-ts` — the Go harness in `tests/conformance/` (real storage
  with the transaction coordinator over SQLite, embedded NATS, sdk/go's
  subscriber) against the suite's fixed service implemented with this SDK,
  installed as the platform's image installs it.
- `make check-ts-descriptors` / `make check-conformance-ts-descriptors` — the
  committed descriptor sets are what `protoc` writes.
