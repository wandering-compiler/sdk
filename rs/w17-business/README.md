# w17 business SDK for Rust

`w17-business` serves a domain's business tier written in Rust. Codegen
writes no Rust for it: it writes `descriptors.binpb` (the domain's protos,
compiled) and `w17-business.json` (the manifest: config keys, storage
services, events). Your crate's `build.rs` compiles the descriptor set with
`w17-business-build` (tonic-prost-build, no `protoc`), you implement the
tonic traits it generates, and this crate serves them with the middleware
sdk/go's generated wrappers apply: header forwarding, deadlines, distributed
transactions, events after commit, `w17.ErrorDetail` errors. A handler is
checked against its proto at compile time.

tokio + tonic 0.14 + prost 0.14. The crate is the binary:

```
/app/business serve --manifest /app/w17-business.json
/app/business health --manifest /app/w17-business.json   # container HEALTHCHECK
```

## Install

There is no package registry. Your crate depends on this repository at the
tag of the project's SDK version — both crates, which live in this directory
(`rs/w17-business` and `rs/w17-business/build`):

```toml
[package]
name = "shop-rs"          # the last segment of the bundle's register path
version = "0.1.0"
edition = "2024"
publish = false

[workspace]               # standalone: never captured by a project workspace

[dependencies]
w17-business = { git = "https://github.com/wandering-compiler/sdk", tag = "rs/v0.1.0-rc.72" }
tonic = "0.14"
prost = "0.14"
tokio = { version = "1", features = ["macros"] }

[build-dependencies]
w17-business-build = { git = "https://github.com/wandering-compiler/sdk", tag = "rs/v0.1.0-rc.72" }
```

`tonic` and `prost` are your own dependencies because the generated code names
them by path; keep the majors this SDK is built on (0.14 for both) — a
mismatch is a compile error on the first message that crosses into the SDK.
Commit `Cargo.lock`: the image builds `--locked`, and refuses a lock whose
`w17-business` is not the tag of the project's `sdk_version` (change the
`tag`, `cargo update -p w17-business -p w17-business-build`, commit).

When a project is generated against a checkout (co-development), or pins a
pseudo-version, the image substitutes this directory for the git source: it
turns the named build context `w17-sdk-rs` into a local git repository tagged
with the tag your `Cargo.lock` names, points the SDK's URL at it
(`url.insteadOf`), and builds without `--locked`. Cargo's `[patch]` cannot do
this — it still fetches the original source, and an unreleased tag is not
there yet.

## What you write

```
business/shop-rs/
  Cargo.toml
  Cargo.lock
  build.rs        w17_business_build::compile()
  src/main.rs     w17_business::main!(register_business);
```

`build.rs`:

```rust
fn main() -> Result<(), Box<dyn std::error::Error>> {
    w17_business_build::compile()
}
```

It finds the manifest — `$W17_BUSINESS_MANIFEST` (the image sets it), else by
walking up to the project's `w17/services/` and taking the
`*-business/w17-business.json` whose `language` is `rust` and whose
`register.path` is this crate — and compiles the descriptor set it names into
`OUT_DIR`. The SDK's own types (the vocabulary, `w17.common.distx`,
`w17.common.files`) and `.google.protobuf` map onto this crate's types, so a
message that crosses into the SDK is one type, not two. The mapping is type
by type: a plugin's package (`w17.contrib.auth`) is generated into your
crate like any other.

`src/main.rs`:

```rust
mod pb { w17_business::include_protos!(); }

use pb::shop::core::checkout_service_server::{CheckoutService, CheckoutServiceServer};
use pb::shop::core::order_mutation_client::OrderMutationClient;
use w17_business::{distx, on_commit, principal, BootError, Clients, Config, Registry, Request, Response, Status, StorageChannel};

w17_business::main!(register_business);

fn register_business(cfg: &Config, registry: &mut Registry, clients: &Clients) -> Result<(), BootError> {
    registry.register(CheckoutServiceServer::new(Checkout {
        orders: OrderMutationClient::new(clients.storage()),
        tx: clients.distributed_transaction()?,
    }))
}

struct Checkout { orders: OrderMutationClient<StorageChannel>, tx: distx::Coordinator }

#[tonic::async_trait]
impl CheckoutService for Checkout {
    async fn place_order(&self, req: Request<pb::shop::core::PlaceOrderReq>) -> Result<Response<pb::shop::core::PlaceOrderResp>, Status> {
        let ctx = w17_business::ctx(&req)?;
        let uid = principal::user_id(&ctx)?;
        let order = distx::run(&ctx, &self.tx, distx::begin_request("shop-postgres"), async |tx_ctx| {
            let order = self.orders.clone()
                .create_order(tx_ctx.request(pb::shop::core::CreateOrderReq { user_id: uid, ..Default::default() }))
                .await?.into_inner();
            let id = order.id.clone();
            on_commit(&tx_ctx, async move { send_mail(id).await }).await.map_err(Status::internal)?;
            Ok::<_, Status>(order)
        }).await?;
        Ok(Response::new(pb::shop::core::PlaceOrderResp { order_id: order.id }))
    }
}
```

`main!` expands to `fn main()` that builds the runtime and hands
`register_business` to `w17_business::run` with the SHA-256 of the descriptor
set the crate was compiled from: a binary built against an older proto
refuses to serve a newer manifest ("rebuild the crate").

### The context, passed explicitly

`w17_business::ctx(&req)` is the request's context: the caller's metadata,
deadline and transaction. Every call you make is built with
`ctx.request(msg)`, which carries it: the SDK's channels relay the verified
principal from it, send its deadline and its transaction, and **refuse a
request without one** (`FAILED_PRECONDITION`). Helpers that change it return
a new one: `paging::forward(&ctx)`, `principal::act_as(&ctx, …)`,
`distx::begin`, `ctx.without_cancel()`, `ctx.with_timeout(d)`.

The middleware ends a call whose deadline passed (`DEADLINE_EXCEEDED`) and a
call whose caller went away by dropping your handler's future — Rust's
cancellation. A transaction a dropped future held is rolled back; work a
committed transaction owes (events, `on_commit`) runs to its end.

### Composing your own methods

Through `registry.self_channel()`, never by calling your struct:

```rust
let me = CheckoutServiceClient::new(registry.self_channel());
me.clone().place_order(ctx.request(req)).await?;
```

The callee runs with its middleware (its event is emitted) and gets your
context unchanged: inside your `distx::run` it joins your transaction and its
event waits for your commit. A direct call skips the emit, and nothing fails —
exactly the Go rule.

### Errors

Return a `Status`:

- `for_user(code, detail_code, message, field)` (or `for_user_dev` with the
  operator's copy apart) refuses with a sentence for a person: the status
  leaves as `Svc.Method: <dev>` with a `w17.ErrorDetail`;
- a status a storage or companion call returned goes out unchanged;
- the SDK's errors (`distx::Error`, `principal::NoPrincipal`, `ConfigError`)
  convert with `?` into `UNKNOWN` with their message;
- a panic answers `INTERNAL` `"<method>: internal error"` and is reported.

### Events

There is nothing to call: the manifest says which methods emit, and the
middleware builds the envelope from the method's request and response
(decoded against the descriptor set) and publishes it to NATS or Redis
Streams after an `OK` answer — or after the root commit when the call ran
inside a transaction, never when it rolled back. Streaming methods carry no
emit, as in Go.

### Paging

`paging::forward(&ctx)` onto the one storage call that serves the page; the
position it answers with comes back on your method's response headers.

### Configuration

`cfg.str("greeting")?`, `cfg.int("limit")?` (`i64`), `cfg.bool("verbose")?`,
`cfg.secret("api_token")?` (a `Secret` that prints `***`; `.reveal()` reads
it). Secrets resolve through env, `.secrets.age`, `.secrets`
(`W17_SECRETS_MODE`).

## Egress clients and files: the business companion

With a `companion` section in the manifest, `clients.egress("quotes")?` is
the channel of the project's egress client (`QuotesApiClient::new(…)`) and
`clients.store("app-exports")?` an upload connection's `FileStore`: both are
served by the domain's business companion, the Go process that runs
`sdk/go/lib/egress` and `sdk/go/lib/kvfs` for this tier. One attempt per call
(the companion retries what is safe), never the storage transaction or
paging headers.

```rust
let handle = store.put(&ctx, "app.example.NoteExport.file", bytes).await?;   // or files::Body::stream(s)
let data = store.read(&ctx, &handle).await?;
let info = store.stat(&ctx, &handle).await?;
store.delete(&ctx, &handle).await?;
```

A put streams 64 KiB chunks and commits only on its end marker: a body that
fails (`files::Error::Body`, your own error), a cancelled call or one past its
deadline stores nothing. `files::is_not_found(&status)` is Go's
`errors.Is(err, kvfs.ErrNotFound)`; `egress::error_info(&status)` reads the
`google.rpc.ErrorInfo` an egress failure carries. Egress hooks are Go
functions: a client that needs one stays on a Go tier.

## API

| | |
|---|---|
| `ctx(&req)`, `Ctx::{request, incoming, outgoing, with_outgoing, with_timeout, without_cancel, err, done}` | the context |
| `principal::{user_id, envelope, scope, label, device_id, data_scope, act_as, forward_to_outgoing}` | the verified caller |
| `paging::forward` | paging, by hand |
| `distx::{run, begin, begin_request, TxHandle, Error}`, `on_commit` | transactions |
| `Clients::{storage, distributed_transaction, egress, store}` | the channels |
| `Registry::{register, self_channel, register_shutdown}` | serving |
| `for_user`, `for_user_dev`, `error_detail` | errors |
| `Config`, `Secret` | configuration |

Health (`grpc.health.v1`), reflection (`<PREFIX>_GRPC_REFLECTION=1`), internal
TLS (`W17_INTERNAL_TLS*`), Sentry (`W17_SENTRY_DSN`) and OpenTelemetry
(`W17_OTEL_*`) are the runtime's, read from the same variables as the Go
tier. On SIGTERM the tier drains under ONE budget, `max(30 s,
shutdown_timeout_seconds)`: NOT_SERVING, the in-flight calls finish, the ones
still running are cancelled, the work committed transactions owe, the
shutdown hooks and the bus close, each bounded by what is left.

## Tests

```
make test-rs-business     # rustfmt, clippy, cargo test, cargo audit
make conformance-rs       # the Go conformance harness against tests/conformance/service
make check-rs-descriptors # src/sdk_descriptors.binpb is what `make rs-descriptors` writes
```
