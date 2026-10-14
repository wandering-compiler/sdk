# storage-proxy

A **contract-blind L7 gRPC proxy** for the wandering-compiler storage
tier. It routes gRPC (HTTP/2) requests to one of *N* identical storage
replicas and **pins every RPC of a distributed transaction to the
replica that opened it**, so a storage tier can scale past one replica
without breaking distributed transactions (the open `*sql.Tx` lives in
one replica's in-memory `txregistry`).

It is a single statically-buildable Rust binary; one published image
serves every project. It is materialized into prod artifacts only when a
storage tier crosses one replica — single-replica / single-`-server`
deployments never see it.

## This proxy never decodes message bodies

**The proxy NEVER unmarshals a request or response protobuf body.** It
inspects only the HTTP/2 `:path` pseudo-header and the `w17-conn-id`
gRPC metadata header, and forwards the request body stream and response
body stream as opaque bytes in both directions. This is what makes it
*build-once*: it needs no per-project protobuf types and never
recompiles when a schema changes. All other headers/trailers — including
W3C `traceparent` / `tracestate` — are forwarded verbatim, so trace
context propagates transparently across the new hop.

## Routing table

The proxy mints an opaque `conn_id` (UUID v4) on `Begin` and keeps an
in-memory `conn_id -> replica_index` affinity map with a per-entry TTL,
plus a round-robin balancer over the replica set. Classification is by
`:path` suffix + the presence of the `w17-conn-id` request header:

| Class | Recognised by | Action |
|---|---|---|
| **Begin** | `:path` ends with `…/W17DistributedTransaction/Begin` | round-robin a replica; **mint** a fresh `conn_id`; pin `conn_id -> replica` (with TTL); forward; **inject `w17-conn-id` into the response headers** (body untouched). |
| **In-tx RPC** | request carries `w17-conn-id` (any other path) | look up the pinned replica and forward. Unknown / expired id → gRPC `FAILED_PRECONDITION` (status 9), no upstream call. |
| **Commit / Rollback** | `:path` ends with `…/Commit` or `…/Rollback`, with `w17-conn-id` | route to the pinned replica, forward, then **evict** the affinity entry. |
| **Other** | no `w17-conn-id` | free round-robin load-balance. |

`conn_id` is **proxy-minted and opaque** — storage never produces or
reads it (`BeginResponse.conn_id` stays empty on the wire). The proxy
owns the token and supplies it out-of-band in response metadata.

Unknown/expired-conn errors are returned as a "trailers-only" gRPC error
(HTTP 200 + `grpc-status` / `grpc-message` leading headers, no body).
Upstream connection failures map to `UNAVAILABLE` (status 14).

## Configuration (env)

| Variable | Default | Meaning |
|---|---|---|
| `W17_PROXY_LISTEN` | `0.0.0.0:50051` | listen address (`host:port`). |
| `W17_PROXY_REPLICAS` | *(required)* | comma-separated replica `host:port` endpoints, e.g. `app-storage-0:50051,app-storage-1:50051`. Empty → fail fast. |
| `W17_PROXY_AFFINITY_TTL_SECS` | `300` | affinity entry TTL in seconds (aligned with the Go `distx.DefaultOrphanTimeout` = 5 min). |
| `W17_OTEL_ENDPOINT` | *(unset)* | OTLP/gRPC endpoint for telemetry, e.g. `http://w17-tracing:4317` (same convention as the rest of the project). **Set** → metrics are exported periodically. **Unset/empty** → the telemetry pipeline is never built (zero overhead). |
| `W17_PROXY_TRACE_SAMPLE_RATIO` | `0.0` | per-RPC span sampling ratio, float in `[0,1]`. `0.0` (default) → **no** per-RPC spans. `> 0` **and** `W17_OTEL_ENDPOINT` set → one sampled span per forwarded RPC. Out-of-range / non-numeric → fail fast. |
| `RUST_LOG` | `info` | log filter (`tracing-subscriber` env filter). |

The resolved config (including whether OTEL is enabled and the sample
ratio) is logged at startup.

## Observability (OTEL profile)

The proxy is a new hop on the storage critical path, so it participates
in tracing — selectively, to keep a body-blind router cheap. **None of
this reads message bodies**: metrics and spans carry only low-cardinality
routing metadata (request *class*, error *reason*, replica *index*, gRPC
*status*) — never the `:path`, the `conn_id`, or any caller data.

Three tiers, by cost:

1. **Trace-context propagation — always on, free.** The proxy forwards
   *all* request/response headers verbatim, including W3C
   `traceparent` / `tracestate`, so the gateway→storage trace is not
   severed at the proxy hop. Unconditional; no config, no per-RPC cost.
   (Asserted by `tests/integration.rs::traceparent_is_forwarded_to_upstream_unchanged`.)
2. **Metrics — always on when `W17_OTEL_ENDPOINT` is set.** Instruments
   are created once at startup; recording on the hot path is a cheap
   atomic add / histogram update. When the endpoint is unset, the meter
   pipeline is never built (no overhead at all).
3. **Per-RPC spans — opt-in + sampled, off by default.** Enabled only
   when `W17_PROXY_TRACE_SAMPLE_RATIO > 0` **and** `W17_OTEL_ENDPOINT` is
   set. Uses a parent-based `TraceIdRatioBased` sampler; each span is
   parented to the incoming `traceparent` (so it joins the existing
   trace), named `storage-proxy <class>`, and carries `w17.proxy.class`,
   `w17.proxy.replica`, and `grpc.status` (0 forward-ok / 9 unknown-conn
   / 14 upstream-unavailable). When the ratio is 0, no span machinery
   touches the hot path.

**Policy in one line:** *propagation always; metrics when an endpoint is
set; per-RPC spans opt-in/sampled — and none of it reads message bodies.*

Metrics (low cardinality by design):

| Metric | Type | Attributes | Meaning |
|---|---|---|---|
| `w17.proxy.routed_total` | counter | `class` (`begin` / `in_tx` / `finish` / `load_balanced` / `unknown_conn`) | RPCs routed, per request class. |
| `w17.proxy.route_errors_total` | counter | `reason` (`unknown_conn` / `upstream_unavailable`) | routing failures, per reason. |
| `w17.proxy.forward_duration_ms` | histogram (ms) | `class` | upstream forward latency. |
| `w17.proxy.affinity_map_size` | observable gauge | — | live `conn_id → replica` affinity entries. |
| `w17.proxy.replicas` | observable gauge | — | configured replica count. |

On SIGINT the meter + tracer providers are flushed and shut down so the
last batch of metrics/spans is exported.

## Build & run (Docker only)

No local Rust toolchain is needed — everything runs in Docker. A named cache
volume holds the cargo registry + `target/` so nothing lands in the checkout.

```sh
# from the root of this repository
docker run --rm \
  -v "$PWD:/work" -w /work/rs/storage-proxy \
  -v w17-cargo-registry:/usr/local/cargo/registry \
  -v w17-cargo-target:/work/rs/storage-proxy/target \
  rust:1-bookworm cargo build --release --locked

# Test (unit + integration)
docker run --rm \
  -v "$PWD:/work" -w /work/rs/storage-proxy \
  -v w17-cargo-registry:/usr/local/cargo/registry \
  -v w17-cargo-target:/work/rs/storage-proxy/target \
  rust:1-bookworm cargo test --locked
```

The published image is `ghcr.io/wandering-compiler/storage-proxy:<version>`,
built for linux/amd64 and linux/arm64 from a `storage-proxy/v<version>` tag of
this repository. To build it locally and run it:

```sh
docker build -t w17-storage-proxy rs/storage-proxy

docker run --rm -p 50051:50051 \
  -e W17_PROXY_REPLICAS=app-storage-0:50051,app-storage-1:50051 \
  w17-storage-proxy
```

## Layout

```
storage-proxy/
├── Cargo.toml
├── Dockerfile          # builder (rust:1-bookworm) → debian:bookworm-slim
├── README.md
├── .gitignore          # /target
├── src/
│   ├── main.rs         # entrypoint: load config, run server, handle SIGINT
│   ├── lib.rs          # crate surface (so routing logic is testable)
│   ├── config.rs       # env parsing + validation
│   ├── lb.rs           # round-robin balancer
│   ├── affinity.rs     # conn_id -> replica map with TTL (injectable clock)
│   ├── router.rs       # pure routing decision (`classify` + `decide`)
│   ├── forward.rs      # hyper h2 transparent body-stream forwarding + telemetry
│   ├── otel.rs         # OTEL providers, metric instruments, sampled spans, propagator
│   └── server.rs       # bind / accept loop / affinity reaper / telemetry lifecycle
└── tests/
    └── integration.rs  # fake h2 upstreams: Begin→follow-up→Commit→evict + traceparent passthrough
```

## Tests

- **Unit** (45 tests): `:path` classification, the routing decision
  function for every class, affinity insert/lookup/evict/TTL-expiry
  (deterministic via an injected clock), round-robin LB, config
  parsing/validation (incl. the OTEL env vars + sample-ratio bounds), and
  the OTEL helpers — stable low-cardinality class labels, a disabled
  (`None`) metrics handle is a safe no-op, and `traceparent` extraction.
- **Integration** (4 tests, `tests/integration.rs`): the real proxy in
  front of fake in-process h2 upstreams asserts that `Begin` mints +
  stamps a `w17-conn-id` and hits one replica, a follow-up with that id
  hits the **same** replica, `Commit` routes to it and **evicts** (a
  reused id is then rejected with `grpc-status: 9` and never reaches an
  upstream), non-tx traffic round-robins, an unknown `conn_id` is
  rejected without an upstream call, and an incoming `traceparent` /
  `tracestate` reaches the upstream **unchanged** (trace continuity).

The routing **decision** is the pure function
[`router::decide`](src/router.rs)
(`fn decide(path, conn_id, &AffinityMap, &RoundRobin, &dyn ConnIdMinter) -> Route`);
it has no I/O, so the whole routing table is unit-tested from
`(path, conn_id, map-state)`.

## Not in this slice

Prod codegen materialization into Compose/k8s (S4) — including wiring
`W17_OTEL_ENDPOINT` into the proxy service env, the Go client-side
`distx.Begin` helper (S3), and the live multi-replica end-to-end proof
(S6). Mutual TLS to replicas is out of scope: the proxy speaks plaintext
prior-knowledge h2 to in-cluster replicas. OTEL (this slice, S5) is done:
trace-context propagation (always), metrics (when an endpoint is set),
and opt-in/sampled per-RPC spans.
