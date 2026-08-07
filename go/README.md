# wandering-compiler SDK — Go

`github.com/wandering-compiler/sdk/go`

The public, consumer-importable Go surface of wandering-compiler: what a
generated service or a hand-written business binary needs at **runtime**, with
zero dependency on the compiler internals. See the parent
[`../README.md`](../README.md) for the cross-language philosophy.

> **Dev-help, not a framework.** Everything here is opt-in. A business layer is
> fully implementable with plain `database/sql` + plain gRPC; import a package
> only when it removes boilerplate. Nothing here owns your control flow.

---

## Start here: `service/runtime.Serve`

One call stands up a wandering-compiler gRPC service binary — observability
(Sentry + OTel), the per-RPC span, your interceptor chain, the gRPC lifecycle
(listen / reflection / health / signal-driven graceful stop), and bounded
teardown. It is the single front door; every generated bundle `main.go`
bootstraps through it.

```go
err := runtime.Serve(ctx, runtime.Config{
    ServiceName:  "app",                          // required
    SentryDSN:    os.Getenv("W17_SENTRY_DSN"),    // empty = Sentry off
    OTelEndpoint: os.Getenv("W17_OTEL_ENDPOINT"), // empty = export off
    Addr:         ":50051",
    Reflection:   true,
    UnaryInterceptors: []grpc.UnaryServerInterceptor{ /* rollback, emit, … */ },
    RegisterServices: func(s *grpc.Server) error {
        pb.RegisterFooServer(s, fooHandler)
        return nil
    },
    Shutdowns: []func(context.Context) error{ /* close pools, drain bus, … */ },
})
```

---

## Package map — organised by audience

The tree is split by **who reaches for it**, so the usage modes don't bleed
together. Dependency direction is layered and acyclic: `service/*`, `lib/*`, and
`tooling/*` are independent of each other; all depend only on the `core/` + `pb/`
foundation.

One distinction up front: `service/` is what **you** write against (stand up +
compose a binary, register your business layer); `lib/` is the runtime the
**compiler emits calls into** — a generated bundle links it, but you almost never
hand-import it.

```
sdk/go/
├── core/       ← foundation both tiers (and other languages, conceptually) share
│   ├── observx/     THE owner of Sentry + OTel: TracerProvider, per-RPC span,
│   │                ReportError correlating Sentry ↔ trace. Configured by runtime.
│   ├── grpcx/       gRPC client utilities: Pool + DialOpts (retry + keepalive
│   │                service-config, the paired defaults).
│   └── grpcerr/     DB-error → gRPC-status mapping (ConstraintInfo: unique / FK /
│                    check → a typed, dialect-portable gRPC error). Generated
│                    storage handlers emit against this.
│
├── service/    ← Go service-host: standing up & composing a service binary.
│   │             Load-bearing for generated main.go + plugins.
│   ├── runtime/     batteries-included Serve (the front door above). The bare
│   │                gRPC lifecycle it composes lives in service/internal/server.
│   ├── bootstrap/   the Component supervisor for composed `-server` binaries.
│   │                RunGraceful drains the transport, THEN closes resources.
│   ├── inprocgrpc/  in-process ClientConn so one `-server` folds its tiers
│   │                (gateway → business → storage) without a network hop.
│   ├── tx/          distributed transactions — the cohesive trio, used together:
│   │   ├── txregistry/    in-memory tx_id → *sql.Tx registry + AdoptTx + DBOrTx
│   │   ├── grpcrollback/  interceptor that auto-rollbacks the caller's tx on error
│   │   └── distx/         the W17DistributedTransaction gRPC server (NewServer)
│   └── secret/      redacting Secret[T] (String() → "***") + the env→age→plain
│                    Resolver (seamless dev, encrypted-at-rest prod).
│
├── lib/        ← runtime helpers the compiler EMITS calls into. A generated
│   │             bundle links these; a hand-written business layer almost never
│   │             imports them directly (that surface is service/ above). Named
│   │             by nature (libraries), not audience — their only consumer is
│   │             the compiler's emitted code. See lib/README.md.
│   ├── eventbus/    the event-bus runtime (NATS / Redis Streams adapters, DLQ).
│   ├── restgw/      the REST↔gRPC gateway runtime (JSON transcode, SSE/WS).
│   ├── i18n/        translation runtime (gettext catalogs → localized strings).
│   ├── acllock/     ACL enforcement (the permission Lock + allocator).
│   ├── kvfs/        KV storage runtime (+ memory / local_fs drivers).
│   ├── kvhash/      KV value hashing.
│   ├── dqlbind/     DQL parameter binding (query params → SQL).
│   ├── paging/      keyset / cursor pagination helpers.
│   ├── grpcclient/  gRPC client dial helpers for the generated stub clients.
│   ├── grpcserver/  gRPC server bootstrap helper used by generated mains.
│   ├── mcp/         MCP server runtime (protobridge).
│   ├── protojsonx/  the w17 JSON dialect (discriminated-union oneof reshape).
│   ├── randstr/     random-id helper (Token) for generated INSERT defaults.
│   └── typere/      typed-regex validation used by generated validators.
│
├── tooling/    ← build/deploy tooling: what w17ctl + the generated e2erunner
│   │             drive. Not imported by a running service.
│   ├── migrate/     per-connection migration-apply orchestration (the w17migrate
│   │                binary): fetch → apply, the wc_migrations ledger, the two-
│   │                phase skirt for CREATE INDEX CONCURRENTLY, and the cross-
│   │                process run-lock (redis/s3) that stops double-apply.
│   ├── datamigrate/ the TRANSFORM_FIELD escape hatch: ADD/REMOVE/RENAME/TRANSFORM
│   │                field ops over JSON/proto via a per-object read-modify-write.
│   ├── fingerprint/ deterministic hash of a target DB's schema state — the drift
│   │                signal migrations and reconcile compare against.
│   ├── certgen/     mints the local dev PKI (self-signed CA + CA-signed leaf) used
│   │                when the internal-mesh TLS switch (W17_INTERNAL_TLS) is on.
│   ├── e2e/         the harness the *generated* e2erunner test module compiles
│   │                against (matchers, interpolation, the gRPC/REST/MCP caller).
│   └── pathguard/   path-containment checks for untrusted relative paths (keeps a
│                    staged file inside its project root).
│
└── pb/         ← proto vocabulary + shared wire types (the foundation everything
    │             else is generated against).
    ├── w17/  (+ w17/{db,pg,mysql,sqlite})   the (w17.*) annotation descriptors
    │                                        plugin authors + codegen consume.
    ├── common/distx/                        W17DistributedTransaction messages
    └── consoleapi / w17compiler / w17registry / applyplan / applyfetch
                                             the console-facing service contracts
                                             (w17ctl talks to the console through
                                             these; a plugin author never needs them).
```

---

## Boundary — what belongs here

Public means: code a consumer importing this module compiles against — a business
handler, a plugin, a generated CLI-command body, a generated e2e test, the proto
annotation vocab. Compiler-internal library code — the bits only the code
generator uses at build time — is **not** shipped here, so importing this module
never drags in compiler internals. Keeping that split honest is what lets this
SDK live in a public repo on its own.
