# w17 business SDK for Python

`w17_business` serves a domain's business tier written in Python. Codegen
writes no Python for it: it writes `descriptors.binpb` (the domain's protos,
compiled) and `w17-business.json` (the manifest: config keys, storage
services, events). The SDK builds the services from them at boot and applies
what sdk/go's generated wrappers do as middleware: header forwarding,
distributed transactions, events after commit, `w17.ErrorDetail` errors.

asyncio only, Python 3.13, on grpclib.

```
pip install "git+https://github.com/wandering-compiler/sdk@py/<version>#subdirectory=py"
python -m w17_business serve --manifest w17-business.json --project-root .
python -m w17_business health --manifest w17-business.json   # container HEALTHCHECK
```

## What you write

The manifest's `register.module` (`<package>.w17`) defines one function, and
your handler classes name their methods **exactly** as the RPCs:

```python
from w17_business import distx, for_user, on_commit, paging, principal, Status

class CheckoutService:
    def __init__(self, cfg, registry, clients):
        self.orders = clients.service("shop.core.OrderMutation")
        self.tx = clients.distributed_transaction()
        self.me = registry          # compose through registry.self(...)
        self.M = registry.pool.message

    async def PlaceOrder(self, ctx, req):
        uid = principal.user_id(ctx)
        async def work(tx_ctx):
            order = await self.orders.CreateOrder(tx_ctx, self.M("shop.core.CreateOrderReq")(user_id=uid))
            await on_commit(tx_ctx, lambda: send_mail(order))   # never for a rollback
            return self.M("shop.core.PlaceOrderResp")(order_id=order.id)
        return await distx.run(ctx, self.tx, distx.begin_request("shop-postgres"), work)  # the connection, by its lock name

def register_business(cfg, registry, clients):
    registry.register("shop.core.CheckoutService", CheckoutService(cfg, registry, clients))
```

- **The context is explicit.** Every handler gets `ctx` first and passes it
  to every call. It carries the incoming metadata, the deadline and the open
  transaction. Helpers return a new context (`paging.forward`,
  `principal.act_as`, `distx.begin`); none changes the one it was given.
- **Compose through `registry.self(Service)`.** A direct call on your class
  skips the method's event, and nothing fails — the Go rule. As in Go it is a
  direct call: the callee gets your ctx unchanged (incoming metadata, the
  open transaction it joins, whatever you put on it). Pass a
  `paging.forward(ctx)` only to the storage call that returns the page, never
  to `self`; and `act_as` on a ctx passed to `self` is not seen by the
  callee's principal readers (it sets the outgoing side), as in Go.
- **Work after a commit outlives the request.** Events and `on_commit` hooks
  of a committed transaction run to their end even when the client hangs up;
  the drain waits for them.
- **Streams:** a server-streaming handler is an async generator; a
  client-streaming one takes an async iterator of requests.
- The boot refuses a handler that lacks a method of its service, a service
  the manifest does not list, and a plugin hook bound to a method with no
  handler.

## Egress clients and files: the business companion

A Go business tier calls the project's egress clients (`proto/clients/<name>/`)
and the domain's upload file stores in-process. A Python tier reaches the same
Go code through the **business companion**, a Go process codegen generates
beside it (its own container, or folded into the domain's composed binary);
the manifest's `companion` section (format 2) says where it is
(`companion.address_env`, default `companion.default_address`) and what it
serves:

```python
from grpclib.exceptions import GRPCError
from w17_business import egress

class ExportService:
    def __init__(self, cfg, registry, clients):
        self.quotes = clients.egress("quotes")          # the client's own gRPC service
        self.exports = clients.store("shop-exports")     # a FileStore
        self.M = registry.pool.message

    async def Export(self, ctx, req):
        try:
            quote = await self.quotes.GetQuote(ctx, self.M("clients.quotes.GetQuoteRequest")(id=req.id))
        except GRPCError as e:
            info = egress.error_info(e)  # ErrorInfo(domain="quotes", reason="UPSTREAM_HTTP_404", metadata={...})
            raise                        # unchanged, as a Go handler returning err
        handle = await self.exports.put(ctx, "shop.core.OrderExport.file", render(quote))
        ...
```

- **Egress** is `sdk/go/lib/egress` verbatim: the companion holds the
  `W17_CLIENT_<NAME>_DSN`, retries what is safe (429/502/503/504 and transport
  errors, idempotent methods only), caps a call at 30 s and answers a status
  whose code describes your situation (404 → `NOT_FOUND`, 409 → `ABORTED`,
  429/5xx → `UNAVAILABLE`, …) with a `google.rpc.ErrorInfo`. Egress hooks
  (`BeforeRequest` / `AfterResponse`) are Go functions: a client that needs one
  stays on a Go tier.
- **Files** are `sdk/go/lib/kvfs`: `put(ctx, field, body)` stores a new object
  of a `writer: BUSINESS` field (full proto name, or `Message.field` when
  unambiguous; `body` bytes-like or an (async) iterable of bytes, sent in 64 KiB
  chunks, capped by the field's `max_size_bytes`) and returns the handle — the
  column's value, byte for byte what Go's `kvfs.Put` returns; `open` streams an
  object, `read` returns it whole, `stat` gives `FileInfo(size, mod_time)`,
  `delete` removes one (only on a connection this tier writes; one the gateway
  writes is read-only). `files.is_not_found(err)` is Go's
  `errors.Is(err, kvfs.ErrNotFound)`. A file is not part of a storage
  transaction, as in Go: delete what you stored when the unit of work fails.
- The companion has a connection of its own: one attempt per call (no retries —
  the egress runtime already retried, and a `put` stream cannot be replayed),
  the principal, trace context, labels and deadline relayed as on a storage
  call, and never `w17-tx-id`, `w17-conn-id` or `x-w17-paging-*`.

## API

| | |
|---|---|
| `Context`, `Metadata`, `background()` | the immutable request context |
| `principal.user_id / envelope / scope / label / device_id / data_scope` | the verified caller, fail-closed (`NoPrincipalError`) |
| `principal.forward_to_outgoing(ctx)`, `principal.act_as(ctx, user, scopes)` | the relay (every storage call applies it) and acting as someone |
| `paging.forward(ctx)` | the incoming page, onto the calls you pass it to only; the page position the call answers with (`x-w17-paging-next` / `-prev` / `-total`) comes back as your method's response headers, so the gateway builds the next and previous cursor without your code knowing the sort |
| `distx.run(ctx, client, req, fn)`, `distx.begin(ctx, client, req)` → `TxHandle.commit/rollback` | transactions; nested ones join |
| `distx.RollbackOnlyError`, `NestedMismatchError`, `HandleFinishedError`, `CommitOutcomeUnknownError` | Go's four transaction errors |
| `on_commit(ctx, fn)` | run after the root commit, never on rollback, at once outside a transaction |
| `for_user(code, detail_code, message, field="", dev=None)` | a status with a `w17.ErrorDetail` |
| `Config.str / int / bool / secret` | the manifest's keys; `Secret` prints `***` |
| `Clients.service(name)`, `Clients.distributed_transaction()` | storage clients (retry on UNAVAILABLE, keepalive, internal TLS) |
| `Clients.egress(name)`, `Clients.store(connection)` → `FileStore.put / open / read / stat / delete` | the companion's egress clients and file stores (above) |
| `egress.error_info(err)` → `ErrorInfo(domain, reason, metadata)` or `None`, `files.is_not_found(err)` | reading a companion error |
| `Registry.register / self / register_shutdown`, `Registry.pool.message(name)` | serving, composing, drain hooks, message classes |

Environment read by the runtime itself, as the Go runtime reads it:
`W17_INTERNAL_TLS` (`_CERT`, `_KEY`, `_CA`, `_CLIENT_CA`), `W17_SENTRY_DSN`,
`W17_OTEL_ENDPOINT`, `W17_OTEL_TLS`, `W17_ENV`, `W17_SERVICE_VERSION`,
`W17_SECRETS_MODE`, `W17_SECRETS_AGE_KEY(_FILE)`.

## Tests

- `make test-py` — unit tests (pytest, in Docker).
- `make conformance-py` — the Go harness in `tests/conformance/` (real storage with
  the transaction coordinator over SQLite, embedded NATS, sdk/go's
  subscriber, the business companion in front of a fake third-party API and
  temp-dir file stores) against the suite's fixed service implemented with
  this SDK.
