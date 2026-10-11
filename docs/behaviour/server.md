# Server startup and lifecycle

How a generated service process starts, serves and stops on the Go SDK: the
component supervisor, the gRPC server and its interceptor chain, bounded stops,
health, internal-mesh TLS, observability bootstrap and the in-process gRPC bridge.
Packages: `go/service/bootstrap`, `go/service/runtime`, `go/service/internal/server`,
`go/service/inprocgrpc`, `go/service/healthcheck`, `go/lib/grpcserver`,
`go/lib/grpcclient` (internal-mesh TLS and dial defaults only), `go/core/lifecycle`,
`go/core/healthz`, `go/core/observx`.

## Process shape

```
main
 └─ bootstrap.Run / RunGraceful(parent ctx, observx.Config, [resources], components...)
     ├─ observx.MustInit            (once per process; error → Run returns it, nothing started)
     ├─ signal.NotifyContext(SIGINT, SIGTERM) → shared ctx
     ├─ goroutine per Component  ── first to return cancels shared ctx
     │    └─ runtime.GRPCComponent → internal/server.Run
     │          listen → grpc.NewServer(opts) → RegisterServices → reflection? → health
     │          serve … ctx.Done → lifecycle.StopGRPC(bounded) → OnShutdown (cfg.Shutdowns, 30s)
     ├─ wait for every component
     ├─ resources[i](ctx 30s)        (RunGraceful only, in order)
     └─ observx.Shutdown(5s)
```

gRPC server options built by `runtime.GRPCComponent`, in order:

1. `grpc.StatsHandler(otelgrpc.NewServerHandler())` — per-RPC server span, outside every interceptor.
2. `grpc.KeepaliveEnforcementPolicy{MinTime: 10s, PermitWithoutStream: true}`.
3. `grpc.KeepaliveParams{Time: 30s, Timeout: 10s}`.
4. `grpc.ChainUnaryInterceptor(recovery, cfg.UnaryInterceptors...)` — recovery is outermost.
5. `grpc.ChainStreamInterceptor(recoveryStream)` — the only stream interceptor.
6. `cfg.ServerOptions...` — appended last.

## Features

| ID | Feature | Code |
|---|---|---|
| S1 | Component supervisor | `go/service/bootstrap/bootstrap.go` — `Run`, `Func`, `Component` |
| S2 | OS signal handling | `go/service/bootstrap/bootstrap.go` — `RunGraceful` |
| S3 | Component panic containment | `go/service/bootstrap/bootstrap.go` — `RunGraceful` |
| S4 | Ordered resource teardown | `go/service/bootstrap/bootstrap.go` — `RunGraceful` |
| S5 | gRPC service component | `go/service/runtime/runtime.go` — `GRPCComponent`, `Serve`, `Config` |
| S6 | Interceptor and server-option order | `go/service/runtime/runtime.go` — `GRPCComponent` |
| S7 | Keepalive pairing | `go/service/runtime/runtime.go` — `GRPCComponent`; `go/lib/grpcclient/grpcclient.go` — `Dial` |
| S8 | gRPC panic recovery interceptors | `go/service/runtime/recover.go` |
| S9 | gRPC server lifecycle | `go/service/internal/server/server.go` — `Run` |
| S10 | Server reflection | `go/service/internal/server/server.go` — `Run` |
| S11 | gRPC health service | `go/service/internal/server/server.go` — `DefaultHealthServer` |
| S12 | Bounded gRPC stop | `go/core/lifecycle/lifecycle.go` — `StopGRPC` |
| S13 | Bounded HTTP stop and drain signal | `go/core/lifecycle/lifecycle.go` — `BindHTTP`, `HTTPStopper`, `Draining`, `WithDrain` |
| S14 | Shutdown timeout from the environment | `go/core/lifecycle/lifecycle.go` — `ShutdownTimeoutFromEnv` |
| S15 | HTTP liveness endpoint | `go/core/healthz/healthz.go` |
| S16 | `health` subcommand | `go/service/healthcheck/healthcheck.go` — `Run`, `Dispatch`, `Options` |
| S17 | Internal-mesh TLS, server side | `go/lib/grpcserver/grpcserver.go` — `TLSServerOption` |
| S18 | Internal-mesh TLS, client side, and dial defaults | `go/lib/grpcclient/grpcclient.go` — `Dial`, `TLSDialOption`, `InternalTLSEnabled` |
| S19 | Observability bootstrap | `go/core/observx/observx.go` — `MustInit`, `Shutdown`, `Tracer` |
| S20 | Error, event and refusal reporting | `go/core/observx/observx.go` — `ReportError`, `ReportEvent`, `ReportRefusal` |
| S21 | In-process gRPC bridge | `go/service/inprocgrpc/inprocgrpc.go` — `New`, `Conn`, `Invoke`, `NewStream` |

## S1. Component supervisor

**Code.** `go/service/bootstrap/bootstrap.go` — `Run`, `Func`, `Component`

**Purpose.** Runs N long-lived components (gRPC server, HTTP server, subscriber loop)
concurrently on one shared context. The first to return, for any reason, stops the rest.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `parent context.Context` | — | cancelling it stops every component |
| `obs observx.Config` | see S19 | an init error is returned before any component starts |
| `comps ...Component` | — | zero components → returns `nil` at once |

No options, environment variables or flags of its own.

**Shares.** Process-global observability state (S19). The shared context is the parent of
every component's context.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S1.1 | Every component MUST run on the same context, each in its own goroutine. | `TestRun_CtxCancel_StopsAllComponents` |
| S1.2 | Cancelling `parent` MUST make every component return; Run returns `nil` when every component returned `nil`. | `TestRun_CtxCancel_StopsAllComponents` |
| S1.3 | When one component returns an error, the shared context MUST be cancelled, siblings drain, and Run returns the first non-nil error. | `TestRun_ComponentError_DrainsSiblingAndReturnsErr` |
| S1.4 | A component that returns `nil` early MUST also cancel its siblings. | no test |
| S1.5 | Run MUST NOT return while any component goroutine is still running. | covered implicitly by the tests above |
| S1.6 | Observability MUST be flushed, bounded at 5s, after components and resources finish, including when a component failed at boot. | no test |
| S1.7 | With no components, Run MUST return `nil` at once. | `TestRun_NoComponents_ReturnsNil` |
| S1.8 | Error reporting MUST NOT deadlock: each component reports at most once into a buffer sized to the component count. | holds by construction; no test |

**Deliberate limits.**
- "First exit stops all": a component with nothing to do must block on `<-ctx.Done()` rather than return.
- No restart policy.
- Every component must return once its context is cancelled; the supervisor waits for all of them.

## S2. OS signal handling

**Code.** `go/service/bootstrap/bootstrap.go` — `RunGraceful`

**Purpose.** One process-wide SIGINT/SIGTERM handler that cancels the shared context.
Components do not install their own.

**Configuration.** None. The handled signals are fixed: SIGINT and SIGTERM.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S2.1 | SIGINT or SIGTERM MUST cancel the shared context exactly once. | no test |
| S2.2 | After `RunGraceful` returns, the signal registration MUST be removed. | no test |

**Deliberate limits.**
- That components do not install their own signal handler is a convention; nothing enforces it.

## S3. Component panic containment

**Code.** `go/service/bootstrap/bootstrap.go` — `RunGraceful`

**Purpose.** A panic inside a component's `Run` goroutine becomes an error plus a sibling
drain instead of a process crash.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S3.1 | A panic in `Component.Run` MUST be recovered, reported through `observx.ReportError` with value and stack, returned as an error, and cancel the shared context. | `TestRun_ComponentPanic_RecoversDrainsSiblingAndReturnsErr` |
| S3.2 | The returned error MUST carry the panic value and stack trace. | `TestRun_ComponentPanic_RecoversDrainsSiblingAndReturnsErr` |

**Deliberate limits.**
- Panics on goroutines a component spawns itself are not recovered (Go cannot do that); they still crash the process.

## S4. Ordered resource teardown

**Code.** `go/service/bootstrap/bootstrap.go` — `RunGraceful`

**Purpose.** In a composed binary, closes shared resources (database pools, event bus) only
after every transport has drained, so in-flight requests keep their pools.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `resources []func(context.Context) error` | none | run in slice order on one fresh 30s context; `nil` entries are skipped |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S4.1 | No resource closer MUST run before every component has returned. | `TestRunGraceful_ResourcesCloseAfterComponentsDrain` |
| S4.2 | Closers MUST run in caller-defined order. | `TestRunGraceful_ResourcesCloseAfterComponentsDrain` |
| S4.3 | `nil` closers MUST be skipped. | `TestRunGraceful_NilResourceSkipped` |
| S4.4 | Closers MUST run even when a component failed. | no test |
| S4.5 | The whole phase MUST be bounded by 30s, provided each closer honours its context. | no test |
| S4.6 | A resource MUST NOT be closed while a request still uses it. | no test |
| S4.7 | A closer's error MUST NOT be lost silently. | no test |

**Deliberate limits.**
- After a forced gRPC stop (S12), handler goroutines are not waited for; this is documented on `lifecycle.StopGRPC`.

## S5. gRPC service component

**Code.** `go/service/runtime/runtime.go` — `GRPCComponent`, `Serve`, `Config`, `ObservxConfig`

**Purpose.** Packages the gRPC server lifecycle as a component: span handler, keepalive,
recovery, caller interceptors, listen/serve, bounded stop, then shutdown hooks.
`Serve` is `bootstrap.Run(ctx, cfg.ObservxConfig(), GRPCComponent(cfg))`.

**Configuration.**

| Field | Default | Invalid or edge values |
|---|---|---|
| `ServiceName` | required by observability init (unless `OTEL_SERVICE_NAME` is set) | empty → `Serve` returns `observx: ServiceName required` |
| `ServiceVersion`, `Environment` | empty is passed through | — |
| `SentryDSN` | empty = error tracker off | a bad DSN makes init fail |
| `OTelEndpoint` | empty → `OTEL_EXPORTER_OTLP_ENDPOINT`; still empty → no trace export | — |
| `SampleRate` | `0` → 1.0 | — |
| `EnableMetrics` | false | — |
| `Addr` | required | empty → `server: address is required`; a bad address fails at listen and is returned |
| `Reflection` | false | — |
| `ShutdownTimeout` | `0` → 10s | — |
| `UnaryInterceptors` | none | — |
| `ServerOptions` | none; appended last | see S6 |
| `RegisterServices` | required | nil → `server: RegisterServices is required`; its error closes the listener and is returned |
| `Shutdowns` | none | each runs after the server stops, on one shared 30s context; nil entries skipped |

**Shares.** Observability globals (through `Serve`); one new `server.DefaultHealthServer()` per component.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S5.1 | The component MUST serve until the context is cancelled, then stop within `ShutdownTimeout`. | `TestGRPCComponent_ServesAndDrains` (stop asserted; bound not asserted) |
| S5.2 | `Shutdowns` MUST run after the server has stopped, in order, with nil entries skipped. | `TestGRPCComponent_ServesAndDrains` (order not asserted) |
| S5.3 | With no `Shutdowns`, nothing runs. | `TestGRPCComponent_NoShutdowns` |
| S5.4 | `ObservxConfig()` MUST copy every observability field. | `TestObservxConfig_Projection` |
| S5.5 | `Serve` MUST run one component under the supervisor and return on context cancel. | `TestServe_StandaloneLifecycle` |
| S5.6 | A clean, context-driven stop MUST return `nil` (the `Component` contract). | no test |
| S5.7 | A shutdown that has to force-stop (S12) MUST still run `Shutdowns`. | no test |
| S5.8 | The server MUST NOT start serving before `RegisterServices` succeeded. | holds by construction; no test |
| S5.9 | The listener MUST NOT leak on a registration error. | `TestRun_RegisterError` |

## S6. Interceptor and server-option order

**Code.** `go/service/runtime/runtime.go` — `GRPCComponent`

**Purpose.** Fixes the order in which per-RPC concerns run on the wire server (see the list
under "Process shape").

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S6.1 | The OpenTelemetry stats handler MUST be installed, so every RPC, including one that fails in an interceptor, has a server span. | no test |
| S6.2 | Recovery MUST be outermost, so it catches panics in every caller-supplied unary interceptor and in the handler. | no test (the interceptor is tested in isolation, S8) |
| S6.3 | `cfg.UnaryInterceptors` MUST run in slice order, outermost first. | no test on the wire server; the in-process twin is `TestConn_WithUnaryInterceptors_ChainOrder` |
| S6.4 | `cfg.ServerOptions` MUST be able to override keepalive and add credentials (they come later in the option list). | no test |
| S6.5 | Every caller-supplied unary interceptor MUST run inside recovery. | no test |

**Deliberate limits.**
- `Config` has no stream-interceptor field (documented on `Config`).
- A tier interceptor added here must also be passed to the in-process bridge (S21) to keep deployment parity (documented on `Config`).

## S7. Keepalive pairing

**Code.** `go/service/runtime/runtime.go` — `GRPCComponent`; `go/lib/grpcclient/grpcclient.go` — `Dial`

**Purpose.** Keeps idle service-to-service connections alive and reaps dead peers. Server and
client settings are paired so pings are never rejected as `too_many_pings`.

**Configuration.**

| Side | Values | Override |
|---|---|---|
| Server | `MinTime=10s`, `PermitWithoutStream=true`, `Time=30s`, `Timeout=10s` | `ServerOptions` |
| Client | `Time=30s`, `Timeout=10s`, `PermitWithoutStream=true` | `Dial` options |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S7.1 | The client ping interval (30s) MUST be at least the server `MinTime` (10s). | `TestDefaultClientKeepalive_PairsWithServerEnforcement` (client side, against a literal) |
| S7.2 | The server MUST ping idle clients every 30s and drop them after 10s without an ack. | no test |

## S8. gRPC panic recovery interceptors

**Code.** `go/service/runtime/recover.go`

**Purpose.** A process-wide net that turns a handler or interceptor panic into a generic
`Internal` status.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S8.1 | A unary panic MUST return `codes.Internal` with the message `"<method>: internal error"`. | `TestRecoveryUnaryInterceptor_RecoversPanic` |
| S8.2 | The panic value and stack MUST NOT reach the client. | `TestRecoveryUnaryInterceptor_RecoversPanic` |
| S8.3 | Value and stack MUST be reported through `observx.ReportError` with the method name. | no test |
| S8.4 | A non-panicking call MUST pass through unchanged. | `TestRecoveryUnaryInterceptor_PassThrough` |
| S8.5 | A stream panic on the handler's goroutine MUST return `Internal`. | `TestRecoveryStreamInterceptor_RecoversPanic` |

**Deliberate limits.**
- Panics on goroutines the handler spawns are not caught (documented in the source).

## S9. gRPC server lifecycle

**Code.** `go/service/internal/server/server.go` — `Run`

**Purpose.** Listen, build the server, register services, optionally add reflection and
health, serve, and stop on context cancel.

**Configuration.** `Addr` (required), `Reflection`, `HealthServer` (nil = no health
service), `ShutdownTimeout` (0 = unbounded at this layer; S5 maps 0 to 10s),
`ServerOptions`, `RegisterServices` (required), `OnShutdown`.

Order: `net.Listen("tcp", Addr)` → `grpc.NewServer` → `RegisterServices` → reflection →
health → `Serve` in a goroutine → wait for context or a serve error.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S9.1 | A missing `Addr` or `RegisterServices` MUST be rejected before listening. | `TestRun_ValidationErrors` |
| S9.2 | A listen failure (port in use, bad address) MUST be returned, wrapped with the address. | `TestRun_ListenError` |
| S9.3 | A registration error MUST close the listener and be returned. | `TestRun_RegisterError` |
| S9.4 | On context cancel: bounded graceful stop, then `OnShutdown`, then return. | `TestRun_GracefulShutdown` |
| S9.5 | If `Serve` fails on its own, `OnShutdown` MUST still run and the serve error is returned. | no test |
| S9.6 | `OnShutdown` MUST run exactly once on every return path after a successful listen and registration. | no test for the serve-error path |
| S9.7 | After a serve error, the server MUST be stopped before `OnShutdown` tears down resources. | no test |
| S9.8 | The server MUST NOT serve any port other than `Addr`. | holds by construction; no test |

**Deliberate limits.**
- TCP only. No `MaxConcurrentStreams`, `MaxConnectionAge` or `MaxConnectionIdle` defaults; the receive size limit and handshake timeout are grpc-go defaults (4 MiB receive). All can be set through `ServerOptions`.

## S10. Server reflection

**Code.** `go/service/internal/server/server.go` — `Run`

**Purpose.** Registers the gRPC reflection service when `Reflection` is true.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S10.1 | Reflection MUST be registered only when the flag is set. | no test |
| S10.2 | Reflection MUST NOT be reachable by untrusted callers without authentication. | no test |

## S11. gRPC health service

**Code.** `go/service/internal/server/server.go` — `DefaultHealthServer`, `Run`

**Purpose.** Registers `grpc.health.v1`, with the root service `""` set to SERVING at construction.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S11.1 | The root service MUST answer SERVING once `Serve` starts. | `TestDefaultHealthServer`; end to end through the S16 tests |
| S11.2 | Health is liveness, not readiness: it MUST NOT consult a dependency. | holds by construction; no test |
| S11.3 | Health SHOULD report NOT_SERVING once shutdown has begun. | no test |
| S11.4 | Health MUST be reachable without credentials. | no test |

## S12. Bounded gRPC stop

**Code.** `go/core/lifecycle/lifecycle.go` — `StopGRPC`

**Purpose.** Graceful stop that always finishes within the timeout, falling back to a hard `Stop`.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `timeout` | — | `<= 0` waits without bound; `> 0` starts a graceful stop and calls `Stop()` when it passes, logging the forced stop to stderr |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S12.1 | With nothing in flight, it MUST return promptly. | `TestStopGRPCReturnsImmediatelyWhenIdle` |
| S12.2 | With an RPC held open, it MUST return within roughly the timeout and abort that RPC. | `TestStopGRPCForcesHardStopOnHeldRPC`, `TestGracefulStop_TimeoutHardStop` |
| S12.3 | `0` and negative values MUST mean unbounded. | `TestStopGRPCZeroTimeoutIsUnbounded`, `TestStopGRPCNegativeTimeoutIsUnbounded`, `TestGracefulStop_ZeroBlocks` |
| S12.4 | The forced path MUST NOT fail the process. | holds by construction (no return value) |
| S12.5 | The graceful-stop goroutine MUST NOT leak. | holds by construction; no test |

**Deliberate limits.**
- After a forced stop, handler goroutines are not waited for (`WaitForHandlers` is deliberately unset). Abandoned handlers can overlap the teardown that follows; documented in the source.

## S13. Bounded HTTP stop and drain signal

**Code.** `go/core/lifecycle/lifecycle.go` — `BindHTTP`, `HTTPStopper`, `Draining`, `WithDrain`

**Purpose.** Bounds an `http.Server` shutdown and tells long-lived handlers (SSE, WebSocket)
to end at the start of the drain.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `BindHTTP(srv)` | — | must be called before serving; wraps any existing `BaseContext` and registers an on-shutdown hook. Called after serving started, earlier listeners lack the drain value (documented misuse, not detected) |
| `Stop(timeout)` / `StopWith(timeout, shutdown)` | — | `timeout <= 0` means unbounded |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S13.1 | The drain channel MUST close when `Shutdown` begins, before waiting on active requests. | `TestStopHTTPDrainAwareStreamEndsImmediately` |
| S13.2 | Idle shutdown MUST return immediately. | `TestStopHTTPIdleReturnsImmediately` |
| S13.3 | When the deadline passes, request contexts MUST be cancelled, the server force-closed, and `Stop` returns `nil`. | `TestStopHTTPForcesCloseAndReportsNoError`, `TestStopHTTPForcedPathCancelsRequestContexts` |
| S13.4 | `Draining(ctx)` MUST return nil when no drain was attached. | `TestDrainingAbsentIsNilChannel` |
| S13.5 | Per-listener base contexts MUST be cancelled on every stop path. | no test for the non-forced path |
| S13.6 | The drain channel MUST be closed at most once, even with repeated `Shutdown` calls or a force-close. | no test |
| S13.7 | An existing `BaseContext` MUST be composed with, not replaced. | no test |
| S13.8 | A server error other than the deadline MUST be returned and MUST NOT force-close. | no test |

**Deliberate limits.**
- Unary handlers must not select on `Draining`; this is a convention only.

## S14. Shutdown timeout from the environment

**Code.** `go/core/lifecycle/lifecycle.go` — `ShutdownTimeoutFromEnv`

**Purpose.** Reads `<PREFIX>_SHUTDOWN_TIMEOUT_SECONDS`. In the SDK the MCP transport uses it
with the prefix `W17_MCP`.

**Configuration.**

| Value | Result |
|---|---|
| unset | 10s |
| unparseable | 10s, not reported |
| `<= 0` | `0`, meaning unbounded |
| `N` | N seconds |

Note that `0` here means unbounded, while `runtime.Config.ShutdownTimeout = 0` means 10s.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S14.1 | Every row of the table above MUST hold. | `TestShutdownTimeoutFromEnv` (unset, 25, nonsense, 0; negative input not covered) |

## S15. HTTP liveness endpoint

**Code.** `go/core/healthz/healthz.go`

**Purpose.** `GET /healthz` returns 200 with `{"status":"ok"}`. No authentication, no backend
calls. Mounted by the MCP listener and the REST gateway.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S15.1 | MUST respond 200 with a JSON content type on any method and request. | covered indirectly by `TestRun_AllListenersAnswer` |
| S15.2 | MUST NOT consult a dependency. | holds by construction; no test |
| S15.3 | MUST NOT require authentication. | holds by construction; no test |

## S16. `health` subcommand

**Code.** `go/service/healthcheck/healthcheck.go` — `Run`, `Dispatch`, `Options`

**Purpose.** A distroless image's HEALTHCHECK: checks every listener the binary opens, on
loopback, and exits non-zero naming the failed ones.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `argv[0] == "health"` | — | runs the check |
| `--timeout` | 3s | `<= 0` → error |
| `Options.Probes` | — | `HTTP` (GET `/healthz` must be 200), `GRPC` (health Check `""` must be SERVING, dialled with the mesh posture), `PublicGRPC` (always plaintext). Empty → error |
| `Options.Final` | false | true: `--help` prints usage and any other bare word is an error instead of starting the server |
| `Options.Getenv` | `os.Getenv` | reads the internal-TLS variables |
| listen address | — | wildcard (`""`, `0.0.0.0`, `::`) → `127.0.0.1`; an explicit host is kept; port `0` or empty → error |

Under mutual TLS the gRPC check presents this node's leaf as its client certificate, so the
leaf needs the ClientAuth extended key usage.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S16.1 | Every check MUST run concurrently under one overall timeout. | `TestRun_TimeoutBoundsAHungListener` |
| S16.2 | Exit MUST be OK only if every listener answered, and the failing ones are named. | `TestRun_AllListenersAnswer`, `TestRun_NamesOnlyTheFailingListener` |
| S16.3 | HTTP without `/healthz` MUST fail. | `TestRun_HTTPWithoutHealthzFails` |
| S16.4 | gRPC NOT_SERVING MUST fail. | `TestRun_GRPCNotServingFails` |
| S16.5 | No configured checks MUST be an error, not success. | `TestRun_NoProbesIsAnError` |
| S16.6 | Bad flags or extra arguments MUST give an error with usage. | `TestRun_FlagErrors` |
| S16.7 | Address forms MUST be mapped as listed. | `TestRun_ListenAddressForms` |
| S16.8 | Under internal TLS the gRPC check MUST verify the chain against the configured CA, naming the server by the leaf's first DNS SAN (else IP SAN), and MUST NOT skip verification. | `TestRun_GRPCUnderInternalTLS` |
| S16.9 | `PublicGRPC` MUST be plaintext even with the switch on. | `TestRun_GRPCUnderInternalTLS` |
| S16.10 | Under `terminated` the gRPC check MUST dial plaintext, as the listener does. | no test |
| S16.11 | Under mutual TLS the check MUST present this node's leaf as its client certificate. | no test |
| S16.12 | `Dispatch` with `Final=false` MUST pass every non-`health` word through; with `Final=true` it owns the command table. | `TestDispatch_OtherWordsFallThrough`, `TestDispatch_FinalOwnsTheCommandTable` |
| S16.13 | Redirects MUST NOT be followed, and the body read MUST be capped at 4 KiB. | no test |
| S16.14 | MUST NOT consult a database. | holds by construction; no test |

## S17. Internal-mesh TLS, server side

**Code.** `go/lib/grpcserver/grpcserver.go` — `TLSServerOption`

**Purpose.** Derives the listener's TLS credentials from the stack-wide switch. Returns
`(opt, true, nil)`, `(nil, false, nil)` or `(nil, false, err)`; the caller appends the option.

**Configuration.**

| Variable | Effect |
|---|---|
| `W17_INTERNAL_TLS` | `on` / `true` / `1` / `yes` (case-insensitive, trimmed) → TLS. `terminated` → plaintext listener (TLS ends at the platform) |
| `W17_INTERNAL_TLS_CERT`, `W17_INTERNAL_TLS_KEY` | both required when on; a missing one or a bad pair → error |
| `W17_INTERNAL_TLS_CLIENT_CA` | set → `RequireAndVerifyClientCert` with that pool; unreadable or no PEM → error; ignored when the switch is off |

Minimum TLS version 1.2.

**Shares.** The switch spelling (`grpcclient.InternalTLSEnabled`) and the certificate
variables with S16 and S18.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S17.1 | Switch off → no option. | `TestSwitchOffNoTLS`, `TestExplicitOff` |
| S17.2 | Switch on without certificate and key → error. | `TestSwitchOnRequiresCertKey` |
| S17.3 | Switch on → server-authenticated TLS. | `TestServerAuthTLSEnabled` |
| S17.4 | A client CA set → mutual TLS; a client without a certificate is rejected in a real handshake. | `TestMutualTLSWithClientCA`, `TestBackendDial_MTLSRejectsClientWithoutCert` |
| S17.5 | A bad client CA → error. | `TestBadClientCAErrors` |
| S17.6 | `terminated` → plaintext listener. | `TestTLSServerOption_TerminatedListensPlain` |
| S17.7 | A listener MUST NOT serve plaintext when TLS was requested. | no test |
| S17.8 | A switch value that is not a recognised spelling MUST NOT silently result in plaintext. | no test |

## S18. Internal-mesh TLS, client side, and dial defaults

**Code.** `go/lib/grpcclient/grpcclient.go` — `Dial`, `TLSDialOption`, `InternalTLSEnabled`, default service config

**Purpose.** One dial path: TLS posture from the same switch, OpenTelemetry propagation, a
retry service config, and keepalive.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `W17_INTERNAL_TLS` | plaintext (h2c) | `on`-family → TLS; `terminated` → TLS against system roots |
| `W17_INTERNAL_TLS_CA` | system roots | unreadable or no PEM → error |
| `W17_INTERNAL_TLS_CERT` / `W17_INTERNAL_TLS_KEY` | no client certificate | both set → presented for mTLS; only one set → error |
| `lookup` | `os.Getenv` in `Dial` | nil passed directly to `TLSDialOption` → plaintext |
| retry | `UNAVAILABLE` only, 3 attempts, backoff 0.1s–1s, throttling 100 tokens at ratio 0.1 | — |
| caller `opts` | appended last | for single-valued options the last one wins |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S18.1 | The default MUST be plaintext. | `TestTLSDialOption_Default_Plain`, `TestTLSDialOption_NilLookup_Plain`, `TestDial_NilLookupDefaults` |
| S18.2 | Switch on (every recognised spelling) or `terminated` → TLS. | `TestTLSDialOption_SwitchOn_TLS`, `TestInternalTLSEnabled_Spellings`, `TestInternalTLS_Terminated` |
| S18.3 | With the switch off, the other variables MUST be ignored. | `TestTLSDialOption_OffIgnoresKnobs` |
| S18.4 | Misconfiguration MUST error at dial-construction time, not on the first RPC. | `TestTLSDialOption_MissingCAFile_Errors`, `TestTLSDialOption_HalfMTLS_Errors`, `TestDial_TLSConfigError_Propagates` |
| S18.5 | Dial MUST NOT block, so a backend that is down at start does not fail startup. | `TestDial_NonBlocking` |
| S18.6 | Real handshakes: a custom CA verifies, an untrusted server fails, mTLS succeeds, a client without a certificate is rejected. | `TestBackendDial_*` (4 tests) |
| S18.7 | Retry MUST cover only `UNAVAILABLE` and MUST be throttled; codes where the server may already have applied a write MUST NOT be retried. | `TestDefaultServiceConfig_*` (3 tests) |
| S18.8 | No path MUST skip certificate verification. | holds by inspection; no test |
| S18.9 | In `terminated` mode the client MUST verify against system roots. | no test |

## S19. Observability bootstrap

**Code.** `go/core/observx/observx.go` — `MustInit`, `Shutdown`, `Tracer`

**Purpose.** One init for the error tracker (Sentry), the OTLP trace exporter, an optional
Prometheus meter provider, and W3C propagation.

**Configuration.**

| Input | Default / effect |
|---|---|
| `ServiceName` | required; overridden by `OTEL_SERVICE_NAME` when that is non-empty |
| `OTelEndpoint` | falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`; 10s construction context |
| `SampleRate` | `0` → 1.0 |
| `SentryDSN` | on when non-empty; traces sample rate fixed at 0.2 |
| `EnableMetrics` | Prometheus reader on the global meter provider |
| `ConfigAttrs` | non-secret, non-empty values → resource attribute `w17.config.<lower(key)>`; secret entries dropped |
| `W17_OBSERVX_DEBUG` | `1` / `true` / `yes` / `on` turns on `ReportEvent`; read at init only |

**Shares.** Package globals (tracer, exporter, meter provider, tracker flag, debug flag,
config) plus the OpenTelemetry globals (tracer provider, propagator, meter provider) and
the global Sentry hub. Every component in the process sees the same state.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S19.1 | An empty service name MUST be an error. | `TestMustInit_ServiceNameRequired` |
| S19.2 | With no exporters, init MUST succeed and be cheap. | `TestMustInit_NoExporters_OK` |
| S19.3 | The full stack MUST initialise with agreeing resource schema URLs. | `TestMustInit_FullStack` |
| S19.4 | Environment overrides for service name and endpoint MUST apply with the documented precedence. | `TestMustInit_OTelServiceNameOverride`, `TestMustInit_OTelServiceNameEmptyKeepsConfig`, `TestMustInit_OTelEndpointFallback`, `TestMustInit_W17OTelEndpointWinsOverFallback` |
| S19.5 | A failure part-way through init MUST tear down whatever was installed (all or nothing). | `TestMustInit_SentryInitError` (first step only) |
| S19.6 | Re-init MUST shut down the previous providers first. | no test |
| S19.7 | `Shutdown` MUST be safe with nothing initialised. | `TestShutdown_NilSafe` |
| S19.8 | Secret config attributes MUST NOT be exported. | `TestConfigResourceAttrsExcludeSecrets` |
| S19.9 | `Tracer()` MUST never return nil. | `TestTracer_ReturnsNonNil` |
| S19.10 | After `Shutdown` or a failed init, the global providers MUST NOT point at shut-down instances. | no test |
| S19.11 | Telemetry export MUST follow the deployment's transport-security posture. | no test |

**Deliberate limits.**
- An exporter that is down at start is not an error: OTLP over gRPC connects lazily, and the batcher drops spans later.

## S20. Error, event and refusal reporting

**Code.** `go/core/observx/observx.go` — `ReportError`, `ReportEvent`, `ReportRefusal`

**Purpose.** Three channels: faults (span error plus error-tracker issue), debug events
(gated by `W17_OBSERVX_DEBUG`), and refusals (always logged, never an issue).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S20.1 | A nil error MUST be a no-op on all three. | `TestReportError_NilNoOp` (`ReportEvent` / `ReportRefusal` not covered) |
| S20.2 | `ReportError` MUST mark the active span as failed and record the error. | `TestReportError_AttachesToActiveOTelSpan` |
| S20.3 | Without the error tracker, `ReportError` MUST log to stderr with CR/LF escaped. | `TestReportError_StderrFallback` (escaping not covered) |
| S20.4 | With the error tracker, the event MUST carry service, trace_id and span_id tags. | `TestReportError_SentryPath` |
| S20.5 | `ReportEvent` MUST be a no-op unless the debug knob is on; when on it adds a span event and never sets error status. | `TestReportEvent_OffByDefault_NoOp`, `TestReportEvent_DebugKnob_StderrFallback`, `TestReportEvent_SpanEvent_NoErrorStatus`, `TestReportEvent_SentryBreadcrumb` |
| S20.6 | `ReportRefusal` MUST always log, add a span event, never set error status and never raise an issue. | no test |
| S20.7 | One request's breadcrumbs MUST NOT be attached to another request's event. | no test |
| S20.8 | Log volume from client-driven refusals MUST be bounded. | no test |

## S21. In-process gRPC bridge

**Code.** `go/service/inprocgrpc/inprocgrpc.go` — `New`, `Conn`, `RegisterService`, `Invoke`, `NewStream`

**Purpose.** Lets a composed binary register tier servers on a `Conn` and call them
directly, with behaviour equivalent to a wire hop.

**Configuration.**

| Option | Effect |
|---|---|
| `WithUnaryInterceptor(i)`, `WithUnaryInterceptors(...)` | the unary interceptor chain |
| `WithStreamInterceptor(i)`, `WithStreamInterceptors(...)` | the stream interceptor chain |
| `RegisterService(sd, impl)` | registers a service; registration must complete before serving (documented contract) |

**Behaviour.**
- Unary `Invoke`: refuses a done context with the matching status before the method lookup;
  unknown method → `Unimplemented`; opens an internal span; recovers a panic into a generic
  `Internal`; rebuilds the context (detaches the transaction scope, relays the principal
  stamps to outgoing, turns outgoing metadata into incoming, clears outgoing); deep-copies
  request and reply; honours `grpc.Header` / `grpc.Trailer` call options and ignores others.
- Streaming `NewStream`: the handler runs on its own goroutine; unbuffered channels give
  backpressure; a separate `finished` channel ends the stream; a panic becomes `Internal`;
  the server context is a child of the caller's and is cancelled on finish.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S21.1 | Calls MUST dispatch directly, flip metadata and apply the interceptor. | `TestConn_DirectDispatch_MetadataAndInterceptor` |
| S21.2 | An unknown unary or stream method MUST return `Unimplemented`. | `TestConn_UnknownMethod_Unimplemented`, `TestConn_Streaming_Unregistered_Unimplemented` |
| S21.3 | A done context MUST be refused as on the wire, before the method lookup. | `TestInvoke_RefusesADoneContextLikeTheWire`, `TestInvoke_DoneContextWinsOverAnUnknownMethod`, `TestNewStream_RefusesADoneContext`, `TestInvoke_LiveContextStillDispatches` |
| S21.4 | A nested call MUST see only what a wire hop delivers: no transaction id, no leak of the caller's incoming metadata, and an unauthenticated caller stays unauthenticated. | `TestConn_NestedCall_SeesOnlyWhatAWireHopWouldDeliver`, `TestConn_Handler_StartsWithACleanOutgoingSide`, `TestConn_CallerWithNoOutgoingMetadata_DoesNotLeakItsIncoming`, `TestConn_UnauthenticatedCaller_StaysUnauthenticated`, `TestConn_Stream_NestedCall_SeesOnlyWhatAWireHopWouldDeliver` |
| S21.5 | The unary chain MUST run outermost first. | `TestConn_WithUnaryInterceptors_ChainOrder`, `TestChainUnary_Single`, `TestConn_WithUnaryInterceptors_Empty`, `TestConn_NoInterceptor_StillDispatches` |
| S21.6 | Stream interceptors MUST run in chain order, and a rejection MUST surface. | `TestConn_StreamInterceptor_Runs`, `TestConn_WithStreamInterceptors_ChainOrder`, `TestConn_StreamInterceptor_RejectionSurfaces`, `TestConn_WithStreamInterceptors_Empty` |
| S21.7 | Headers and trailers MUST work for unary and streams; `Header()` MUST be released when the handler ends silently. | `TestConn_Invoke_HonoursHeaderAndTrailerCallOptions`, `TestConn_Invoke_NoHeaderSet_IsEmptyNotBroken`, `TestConn_Stream_HeaderAndTrailer`, `TestConn_Stream_HeaderUnblocksWhenHandlerEndsSilently` |
| S21.8 | A stream handler panic MUST become `Internal`, and the process survives. | `TestConn_Streaming_PanicRecovered` |
| S21.9 | A unary handler panic MUST become a generic `Internal`, and the process survives. | no test in this package; the helper is covered by `TestRecoverPanic_AssignsInternalStatus` (`go/core/grpcerr`) |
| S21.10 | A late send from a spawned goroutine MUST return an error instead of panicking. | `TestServerStream_LateSendFromASpawnedGoroutineDoesNotPanic` |
| S21.11 | A client send after the handler returned MUST NOT hang. | `TestConn_Streaming_SendAfterServerDoneDoesNotHang` |
| S21.12 | Caller cancellation MUST reach the handler. | `TestConn_Streaming_CallerCancel` |
| S21.13 | A handler error MUST become the caller's final status. | `TestConn_Streaming_TrailerErrorPropagates`, `TestInvoke_HandlerError` |
| S21.14 | Context errors on streams MUST be delivered as gRPC status errors (`Canceled` / `DeadlineExceeded`), as on the wire. | no test |
| S21.15 | Messages MUST be deep-copied, so neither side can mutate the other's value. | no direct test; error paths covered by `TestCloneMsg_NonProto`, `TestCopyProto_Errors` |
| S21.16 | A nested call MUST NOT be granted more than a wire hop. | covered by the S21.4 tests |

**Deliberate limits.**
- A stream goroutine stays alive until the caller cancels its context or reads to the end — the same contract as grpc-go.
- No message-size limit and no deadline injection: there is no wire.
