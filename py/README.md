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
| `Registry.register / self / register_shutdown`, `Registry.pool.message(name)` | serving, composing, drain hooks, message classes |

Environment read by the runtime itself, as the Go runtime reads it:
`W17_INTERNAL_TLS` (`_CERT`, `_KEY`, `_CA`, `_CLIENT_CA`), `W17_SENTRY_DSN`,
`W17_OTEL_ENDPOINT`, `W17_OTEL_TLS`, `W17_ENV`, `W17_SERVICE_VERSION`,
`W17_SECRETS_MODE`, `W17_SECRETS_AGE_KEY(_FILE)`.

## Tests

- `make test-py` — unit tests (pytest, in Docker).
- `make conformance-py` — the Go harness in `tests/conformance/` (real storage with
  the transaction coordinator over SQLite, embedded NATS, sdk/go's
  subscriber) against the suite's fixed service implemented with this SDK.
