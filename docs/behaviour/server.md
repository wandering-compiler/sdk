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
     ├─ SIGINT / SIGTERM / SIGHUP → cancel shared ctx; a second one during the drain → exit status 1
     ├─ goroutine per Component  ── first to return cancels shared ctx
     │    └─ runtime.GRPCComponent → internal/server.Run
     │          listen → grpc.NewServer(opts) → RegisterServices → reflection (guarded, S10)? → health
     │          serve … ctx.Done → health NOT_SERVING → lifecycle.StopGRPC(bounded) → OnShutdown (cfg.Shutdowns, 30s) → nil
     ├─ wait for every component      ┐ bounded together by W17_SHUTDOWN_DEADLINE_SECONDS (20s)
     ├─ resources[i](ctx 30s)         ┘ (RunGraceful only, in order; errors reported)
     └─ observx.Shutdown(5s)
```

gRPC server options built by `runtime.GRPCComponent`, in order:

1. `grpc.StatsHandler(otelgrpc.NewServerHandler())` — per-RPC server span, outside every interceptor.
2. `grpc.KeepaliveEnforcementPolicy{MinTime: 10s, PermitWithoutStream: true}`.
3. `grpc.KeepaliveParams{Time: 30s, Timeout: 10s}`.
4. `grpcserver.TLSServerOption` from the process environment, when the internal-TLS switch is on (S17).
5. `grpc.UnaryInterceptor(recovery)` and `grpc.StreamInterceptor(recoveryStream)` — grpc-go's singular
   interceptor slot, which it always runs ahead of every chained interceptor, so recovery is outermost.
   Recovery also gives each request its own error-tracker scope (S8, S20).
6. `grpc.ChainUnaryInterceptor(cfg.UnaryInterceptors...)` — when the slice is non-empty.
7. `cfg.ServerOptions...` — appended last; a singular interceptor option here is refused at start.

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
| `W17_SHUTDOWN_DEADLINE_SECONDS` (environment, `bootstrap.EnvShutdownDeadline`) | 20s (`bootstrap.DefaultShutdownDeadline`) | read once at start; `0` or negative → no bound; not a whole number → logged, default applies |

**Shares.** Process-global observability state (S19). The shared context is the parent of
every component's context.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S1.1 | Every component MUST run on the same context, each in its own goroutine. | `TestRun_CtxCancel_StopsAllComponents` |
| S1.2 | Cancelling `parent` MUST make every component return; Run returns `nil` when every component returned `nil`. | `TestRun_CtxCancel_StopsAllComponents` |
| S1.3 | When one component returns an error, the shared context MUST be cancelled, siblings drain, and Run returns the first non-nil error. | `TestRun_ComponentError_DrainsSiblingAndReturnsErr` |
| S1.4 | A component that returns `nil` early MUST also cancel its siblings. | no test |
| S1.5 | Run MUST NOT return while any component goroutine is still running, except when the shutdown deadline (S1.9) has passed. | covered implicitly by the tests above; `TestRun_ShutdownDeadlineZeroIsUnbounded` |
| S1.6 | Observability MUST be flushed, bounded at 5s, after components and resources finish, including when a component failed at boot. | no test |
| S1.7 | With no components, Run MUST return `nil` at once. | `TestRun_NoComponents_ReturnsNil` |
| S1.8 | Error reporting MUST NOT deadlock: each component reports at most once into a buffer sized to the component count. | holds by construction; no test |
| S1.9 | From the moment shutdown starts (the shared context is cancelled for any reason), the component drain and the resource teardown together MUST finish within the shutdown deadline; on overrun Run returns an error naming `W17_SHUTDOWN_DEADLINE_SECONDS` without waiting for the components, the error is reported through `observx.ReportError`, and the teardown context is cancelled. | `TestRun_ShutdownDeadlineBoundsAStuckComponent`, `TestRunGraceful_ShutdownDeadlineBoundsResourceTeardown` |
| S1.10 | On overrun, a component error reported before the deadline MUST be returned together with the deadline error. | `TestRun_ShutdownDeadlineKeepsTheComponentError` |
| S1.11 | A deadline of `0` MUST remove the bound; an unparseable value MUST be logged and the default applied. | `TestRun_ShutdownDeadlineZeroIsUnbounded` (unparseable value not covered) |
| S1.12 | The default deadline plus the 5s observability flush MUST end before a 30s orchestrator grace period. | `TestDefaultShutdownDeadline_FitsTheCommonGracePeriod` |

**Deliberate limits.**
- "First exit stops all": a component with nothing to do must block on `<-ctx.Done()` rather than return.
- No restart policy.
- Every component must return once its context is cancelled; the supervisor waits for all of them until the shutdown deadline, and not after it.
- After a deadline overrun, the stuck components keep running until the process exits; the caller of Run is expected to exit.
- Each component's own drain bound (for a gRPC component, `runtime.Config.ShutdownTimeout`, default 10s) must be below the deadline, or the deadline cuts it short. Under a grace period shorter than 25s the deadline must be lowered.

## S2. OS signal handling

**Code.** `go/service/bootstrap/bootstrap.go` — `RunGraceful`

**Purpose.** One process-wide SIGINT/SIGTERM/SIGHUP handler that cancels the shared context.
Components do not install their own. A second signal during the drain ends the process at once.

**Configuration.** None. The handled signals are fixed: SIGINT, SIGTERM and SIGHUP. SIGQUIT
keeps Go's default behaviour (goroutine dump and exit).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S2.1 | The first SIGINT, SIGTERM or SIGHUP MUST cancel the shared context exactly once. | `TestRun_SecondSignalExitsImmediately` (first SIGTERM starts the drain; SIGINT and SIGHUP not covered) |
| S2.2 | After `RunGraceful` returns, the signal registration MUST be removed. | no test |
| S2.3 | A second SIGINT, SIGTERM or SIGHUP during the drain MUST exit the process at once with status 1, whether or not a shutdown deadline is set. | `TestRun_SecondSignalExitsImmediately` (real SIGTERM to a child process; SIGINT and SIGHUP not covered) |

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
| `resources []func(context.Context) error` | none | run in slice order on one fresh 30s context, which is cancelled early when the shutdown deadline (S1.9) passes; `nil` entries are skipped |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S4.1 | No resource closer MUST run before every component has returned. | `TestRunGraceful_ResourcesCloseAfterComponentsDrain` |
| S4.2 | Closers MUST run in caller-defined order. | `TestRunGraceful_ResourcesCloseAfterComponentsDrain` |
| S4.3 | `nil` closers MUST be skipped. | `TestRunGraceful_NilResourceSkipped` |
| S4.4 | Closers MUST run even when a component failed. | no test |
| S4.5 | The whole phase MUST be bounded by 30s and by the shutdown deadline (S1.9), whose passing cancels the closers' context; Run returns at the deadline even when a closer ignores its context. | `TestRunGraceful_ShutdownDeadlineBoundsResourceTeardown` (30s bound not covered) |
| S4.6 | A resource MUST NOT be closed while a request still uses it. | `TestGRPCComponent_AbandonedHandlerHoldsResources`, `TestRunGraceful_HeldResourcesAreNotClosed`, `TestGRPCComponent_ShutdownHooksWaitForHandlersAfterForcedStop` |
| S4.7 | A closer's error MUST be reported through `observx.ReportError`; later closers still run and Run's result stays the components' verdict. | `TestRunGraceful_ResourceCloserErrorIsReported` |

**Deliberate limits.**
- After a forced gRPC stop (S12) the component waits a short grace (5s) for handlers to return. A handler still running then is abandoned: the component's hooks are skipped, it calls `bootstrap.HoldResources`, and the closers are skipped and reported. The process exits next, which releases them; what is lost is a clean close.

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
| `OTelTLS` | empty → `W17_OTEL_TLS`; still empty → follows `W17_INTERNAL_TLS` (S19) | an unrecognised value makes init fail |
| `SampleRate` | `0` → 1.0 | outside 0..1 or NaN → init fails and `Serve` returns the error |
| `EnableMetrics` | false | — |
| `Addr` | required | empty → `server: address is required`; a bad address fails at listen and is returned |
| `Reflection` | false | true without `ReflectionAuth` or `ReflectionUnauthenticated` → start-up error naming both (S10) |
| `ReflectionAuth` | nil | runs on every reflection stream (S10) |
| `ReflectionUnauthenticated` | false | true → reflection served without authentication, logged at start |
| `ShutdownTimeout` | `0` → 10s | negative (`lifecycle.Unbounded`) → no bound |
| `UnaryInterceptors` | none | — |
| `ServerOptions` | none; appended last | a singular `grpc.UnaryInterceptor` / `grpc.StreamInterceptor` → start-up error (S6) |
| `RegisterServices` | required | nil → `server: RegisterServices is required`; its error closes the listener and is returned |
| `Shutdowns` | none | each runs after the server stops, on one shared 30s context; nil entries skipped; an error is reported and the later hooks still run |
| `W17_INTERNAL_TLS*` (environment) | off | read at start through `grpcserver.TLSServerOption` (S17); a misconfigured or unrecognised value → start-up error before listening |

**Shares.** Observability globals (through `Serve`); one new `server.DefaultHealthServer()` per component.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S5.1 | The component MUST serve until the context is cancelled, then stop within `ShutdownTimeout`. | `TestGRPCComponent_ServesAndDrains` (stop asserted; bound not asserted) |
| S5.2 | `Shutdowns` MUST run after the server has stopped, in order, with nil entries skipped. | `TestGRPCComponent_ServesAndDrains`, `TestGRPCComponent_ShutdownHookErrorIsReported` |
| S5.3 | With no `Shutdowns`, nothing runs. | `TestGRPCComponent_NoShutdowns` |
| S5.4 | `ObservxConfig()` MUST copy every observability field, including `OTelTLS`. | `TestObservxConfig_Projection` |
| S5.5 | `Serve` MUST run one component under the supervisor and return on context cancel. | `TestServe_StandaloneLifecycle` |
| S5.6 | A clean, context-driven stop MUST return `nil` (the `Component` contract) from `GRPCComponent`, `Serve` and `bootstrap.Run` with a gRPC component. | `TestGRPCComponent_CleanStopReturnsNil` |
| S5.7 | A shutdown that has to force-stop (S12) MUST still run `Shutdowns` once the handlers have returned; when a handler is still running after the grace, the hooks MUST be skipped and reported rather than run under it (S4.6). | `TestGRPCComponent_ShutdownHooksWaitForHandlersAfterForcedStop`, `TestGRPCComponent_AbandonedHandlerHoldsResources` |
| S5.8 | The server MUST NOT start serving before `RegisterServices` succeeded. | holds by construction; no test |
| S5.9 | The listener MUST NOT leak on a registration error. | `TestRun_RegisterError` |
| S5.10 | A failing shutdown hook MUST be reported through `observx.ReportError`, and the later hooks still run. | `TestGRPCComponent_ShutdownHookErrorIsReported` |
| S5.11 | With the internal-TLS switch on, the listener MUST serve TLS and MUST NOT serve a plaintext client. | `TestGRPCComponent_ServesInternalTLS` |
| S5.12 | A misconfigured switch (on without certificate and key, or an unrecognised value) MUST fail the start with an error naming `W17_INTERNAL_TLS`. | `TestGRPCComponent_InternalTLSMisconfigurationFailsStart` |
| S5.13 | `ShutdownTimeout` `0` MUST mean 10s and a negative value MUST mean unbounded, so the opt-out from S14 stays unbounded. | `TestShutdownTimeout_EnvOptOutStaysUnbounded` |
| S5.14 | `Reflection`, `ReflectionAuth` and `ReflectionUnauthenticated` MUST reach the server, so an unguarded `Reflection: true` fails the component's start (S10.2). | `TestGRPCComponent_UnguardedReflectionIsRefused` |

## S6. Interceptor and server-option order

**Code.** `go/service/runtime/runtime.go` — `GRPCComponent`

**Purpose.** Fixes the order in which per-RPC concerns run on the wire server (see the list
under "Process shape").

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S6.1 | The OpenTelemetry stats handler MUST be installed, so every RPC, including one that fails in an interceptor, has a server span. | no test |
| S6.2 | Recovery MUST be outermost (grpc-go's singular interceptor slot), so it catches panics in every caller-supplied unary interceptor and in the handler, and the server keeps serving. | `TestGRPCComponent_RecoveryWrapsCallerInterceptors` |
| S6.3 | `cfg.UnaryInterceptors` MUST run in slice order, outermost first. | no test on the wire server; the in-process twin is `TestConn_WithUnaryInterceptors_ChainOrder` |
| S6.4 | `cfg.ServerOptions` MUST be able to override keepalive and add credentials (they come later in the option list). | no test |
| S6.5 | Every caller-supplied unary interceptor, in `cfg.UnaryInterceptors` or chained in through `ServerOptions`, MUST run inside recovery. | `TestGRPCComponent_RecoveryWrapsCallerInterceptors` |
| S6.6 | A singular `grpc.UnaryInterceptor` or `grpc.StreamInterceptor` in `ServerOptions` MUST be refused at start with an error pointing at the chained option, not by a process panic. | `TestGRPCComponent_SingularInterceptorOptionIsRefused`, `TestRun_ConflictingInterceptorOptionsAreAnError` |

**Deliberate limits.**
- `Config` has no stream-interceptor field (documented on `Config`); extra interceptors go in through `grpc.ChainUnaryInterceptor` / `grpc.ChainStreamInterceptor` in `ServerOptions`, since the singular slot belongs to recovery.
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
| S8.6 | Every unary and stream request MUST run with its own error-tracker scope (`observx.WithRequestScope`), so breadcrumbs recorded while serving it are attached only to its own events (S20.7). | `TestRecoveryInterceptors_GiveEachRequestItsOwnScope` |

**Deliberate limits.**
- Panics on goroutines the handler spawns are not caught (documented in the source).

## S9. gRPC server lifecycle

**Code.** `go/service/internal/server/server.go` — `Run`

**Purpose.** Listen, build the server, register services, optionally add reflection and
health, serve, and stop on context cancel.

**Configuration.** `Addr` (required), `Reflection`, `ReflectionAuth`,
`ReflectionUnauthenticated` (S10), `HealthServer` (nil = no health service),
`ShutdownTimeout` (0 = unbounded at this layer; S5 maps 0 to 10s), `ServerOptions`,
`RegisterServices` (required), `OnShutdown`.

Order: validate (including the reflection guard) → `net.Listen("tcp", Addr)` →
`grpc.NewServer` → `RegisterServices` → reflection → health → `Serve` in a goroutine →
wait for context or a serve error.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S9.1 | A missing `Addr` or `RegisterServices` MUST be rejected before listening. | `TestRun_ValidationErrors` |
| S9.2 | A listen failure (port in use, bad address) MUST be returned, wrapped with the address. | `TestRun_ListenError` |
| S9.3 | A registration error MUST close the listener and be returned. | `TestRun_RegisterError` |
| S9.4 | On context cancel: bounded graceful stop, then `OnShutdown`, then return `nil`. | `TestRun_GracefulShutdown` |
| S9.5 | If `Serve` fails on its own, `OnShutdown` MUST still run and the serve error is returned, wrapped with the address. | `TestRun_ServeErrorStopsServerBeforeOnShutdown` (address not asserted) |
| S9.6 | `OnShutdown` MUST run exactly once on every return path after a successful listen and registration. | `TestRun_GracefulShutdown`, `TestRun_ServeErrorStopsServerBeforeOnShutdown` (run asserted; count not asserted) |
| S9.7 | After a serve error, the server MUST be stopped before `OnShutdown` tears down resources. | `TestRun_ServeErrorStopsServerBeforeOnShutdown` |
| S9.8 | The server MUST NOT serve any port other than `Addr`. | holds by construction; no test |
| S9.9 | An option set grpc-go rejects (two singular interceptors) MUST close the listener and be returned as an error. | `TestRun_ConflictingInterceptorOptionsAreAnError` |

**Deliberate limits.**
- TCP only. No `MaxConcurrentStreams`, `MaxConnectionAge` or `MaxConnectionIdle` defaults; the receive size limit and handshake timeout are grpc-go defaults (4 MiB receive). All can be set through `ServerOptions`.

## S10. Server reflection

**Code.** `go/service/internal/server/server.go` — `Run`

**Purpose.** Registers the gRPC reflection service (v1 and v1alpha) when `Reflection` is
true. Reflection lists every service and message schema, so it is served only behind a
guard or after an explicit statement that it may be served to anyone.

**Configuration.**

| Field | Default | Invalid or edge values |
|---|---|---|
| `Reflection` | false | true needs one of the two fields below; with neither, `Run` returns an error naming both, before listening |
| `ReflectionAuth func(ctx) error` | nil | runs on every reflection stream; a plain error → `Unauthenticated` with a fixed message; a gRPC status error → passed through |
| `ReflectionUnauthenticated` | false | true → reflection served without authentication; logged at start. Ignored when `ReflectionAuth` is set |

The same three fields exist on `runtime.Config` (S5).

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S10.1 | Reflection MUST be registered only when the flag is set. | `TestRun_ReflectionRegisteredOnlyWhenSet` |
| S10.2 | Reflection MUST NOT be reachable by untrusted callers without authentication: `Reflection: true` with neither `ReflectionAuth` nor `ReflectionUnauthenticated` MUST be refused at start, before listening. | `TestRun_UnguardedReflectionIsRefused`, `TestGRPCComponent_UnguardedReflectionIsRefused` |
| S10.3 | With `ReflectionAuth` set, the guard MUST run on every reflection stream of both v1 and v1alpha before any answer. | `TestRun_ReflectionAuthGuardsBothVersions` |
| S10.4 | A plain error from the guard MUST become `Unauthenticated` without its text reaching the caller; a gRPC status error MUST pass through unchanged. | `TestRun_ReflectionAuthGuardsBothVersions`, `TestRun_ReflectionAuthStatusPassesThrough` |
| S10.5 | `ReflectionUnauthenticated` MUST serve reflection and be logged at start. | `TestRun_ReflectionRegisteredOnlyWhenSet` (log line not asserted) |

**Deliberate limits.**
- The SDK enforces that a guard or an explicit opt-in exists; it does not choose the guard. A generated or hand-written `main` that enables reflection must pass `ReflectionAuth` (or `ReflectionUnauthenticated`) itself.

## S11. gRPC health service

**Code.** `go/service/internal/server/server.go` — `DefaultHealthServer`, `Run`

**Purpose.** Registers `grpc.health.v1`, with the root service `""` set to SERVING at construction.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S11.1 | The root service MUST answer SERVING once `Serve` starts. | `TestDefaultHealthServer`; end to end through the S16 tests |
| S11.2 | Health is liveness, not readiness: it MUST NOT consult a dependency. | holds by construction; no test |
| S11.3 | Health MUST report NOT_SERVING for every service once shutdown has begun, before the drain finishes. | `TestRun_HealthReportsNotServingOnceDrainStarts` |
| S11.4 | Health MUST be reachable without credentials. | `TestHealth_ReachableWithoutCredentialsPastCallerInterceptors`, `TestHealth_StaysInsideRecovery` |

## S12. Bounded gRPC stop

**Code.** `go/core/lifecycle/lifecycle.go` — `StopGRPC`

**Purpose.** Graceful stop that always finishes within the timeout, falling back to a hard `Stop`.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `timeout` | — | `<= 0` waits without bound; `> 0` starts a graceful stop and calls `Stop()` when it passes, logging the forced stop to stderr; `Stop()` gets one more `timeout`, after which the call returns without it |

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S12.1 | With nothing in flight, it MUST return promptly. | `TestStopGRPCReturnsImmediatelyWhenIdle` |
| S12.2 | With an RPC held open, it MUST return within roughly the timeout and abort that RPC; when the hard stop itself does not return (a handler ignoring its cancelled context), it MUST still return within twice the timeout. | `TestStopGRPCForcesHardStopOnHeldRPC`, `TestGracefulStop_TimeoutHardStop`, `TestStopGRPCBoundsAHardStopThatDoesNotReturn` |
| S12.3 | `0` and negative values MUST mean unbounded. | `TestStopGRPCZeroTimeoutIsUnbounded`, `TestStopGRPCNegativeTimeoutIsUnbounded`, `TestGracefulStop_ZeroBlocks` |
| S12.4 | The forced path MUST NOT fail the process. | holds by construction (no return value) |
| S12.5 | The graceful-stop and hard-stop goroutines MUST NOT outlive the handlers they wait for. | holds by construction; no test |

**Deliberate limits.**
- After a forced stop, handler goroutines are not waited for (`WaitForHandlers` is deliberately unset), and a hard stop past its bound is left to finish on its own. The component (S5) gives handlers a grace and skips the teardown under one that is still running (S4.6).

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
| `<= 0` | `lifecycle.Unbounded` (negative), meaning unbounded |
| `N` | N seconds |

The opt-out is negative because `runtime.Config.ShutdownTimeout = 0` means 10s; the result can be
handed to that field unchanged.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S14.1 | Every row of the table above MUST hold. | `TestShutdownTimeoutFromEnv` |
| S14.2 | The opt-out MUST stay unbounded when handed to `runtime.Config.ShutdownTimeout`. | `TestShutdownTimeout_EnvOptOutStaysUnbounded` |

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
leaf needs the ClientAuth extended key usage. When `W17_INTERNAL_TLS_CLIENT_CA` is set and the
leaf lacks it, the check fails before dialing with an error that names the cause and the fix.

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
| S16.10 | Under `terminated` the gRPC check MUST dial plaintext, as the listener does. | `TestRun_GRPCUnderTerminatedIsPlaintext` |
| S16.11 | Under mutual TLS the check MUST present this node's leaf as its client certificate. | `TestRun_GRPCUnderMutualTLS` |
| S16.12 | `Dispatch` with `Final=false` MUST pass every non-`health` word through; with `Final=true` it owns the command table. | `TestDispatch_OtherWordsFallThrough`, `TestDispatch_FinalOwnsTheCommandTable` |
| S16.13 | Redirects MUST NOT be followed, and the body read MUST be capped at 4 KiB. | no test |
| S16.14 | MUST NOT consult a database. | holds by construction; no test |
| S16.15 | An unrecognised `W17_INTERNAL_TLS` value MUST fail the check with an error naming it, as the listener refuses it. | `TestRun_GRPCUnrecognisedTLSSwitchFails` |
| S16.16 | Under mutual TLS, a leaf whose extended key usage lacks ClientAuth MUST fail the check before dialing, with an error naming the missing usage and how to reissue the leaf. | `TestRun_GRPCUnderMutualTLS_ServerOnlyLeafIsDiagnosed` |

## S17. Internal-mesh TLS, server side

**Code.** `go/lib/grpcserver/grpcserver.go` — `TLSServerOption`

**Purpose.** Derives the listener's TLS credentials from the stack-wide switch. Returns
`(opt, true, nil)`, `(nil, false, nil)` or `(nil, false, err)`; the caller appends the option.
`runtime.GRPCComponent` (S5) applies it itself from the process environment.

**Configuration.**

| Variable | Effect |
|---|---|
| `W17_INTERNAL_TLS` | `on` / `true` / `1` / `yes` (case-insensitive, trimmed) → TLS. `terminated` → plaintext listener (TLS ends at the platform). Unset or `off` / `false` / `0` / `no` → no option. Any other value → error naming the variable, the value and every accepted spelling (`grpcclient.CheckInternalTLS`) |
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
| S17.7 | A listener MUST NOT serve plaintext when TLS was requested. | `TestGRPCComponent_ServesInternalTLS` |
| S17.8 | A switch value that is not a recognised spelling MUST be an error, not plaintext. | `TestTLSServerOption_UnrecognisedSwitchIsRefused`, `TestCheckInternalTLS` |

## S18. Internal-mesh TLS, client side, and dial defaults

**Code.** `go/lib/grpcclient/grpcclient.go` — `Dial`, `TLSDialOption`, `ClientTLSConfig`, `InternalTLSEnabled`, default service config

**Purpose.** One dial path: TLS posture from the same switch, OpenTelemetry propagation, a
retry service config, and keepalive.

**Configuration.**

| Setting | Default | Invalid or edge values |
|---|---|---|
| `W17_INTERNAL_TLS` | plaintext (h2c) | `on`-family → TLS; `terminated` → TLS against system roots only; `off`-family → plaintext; any other value → error from `TLSDialOption`, `ClientTLSConfig` and `Dial` |
| `W17_INTERNAL_TLS_CA` | system roots | unreadable or no PEM → error; ignored under `terminated`, with a warning |
| `W17_INTERNAL_TLS_CERT` / `W17_INTERNAL_TLS_KEY` | no client certificate | both set → presented for mTLS; only one set → error; ignored under `terminated`, with a warning |
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
| S18.9 | In `terminated` mode the client MUST verify against the system roots only: `W17_INTERNAL_TLS_CA` and the client pair MUST be ignored, and a warning MUST name the ignored variables. | `TestClientTLSConfig_TerminatedUsesSystemRootsOnly`, `TestInternalTLS_Terminated` |
| S18.10 | An unrecognised switch value MUST be refused at dial construction with an error naming the variable, the value and the accepted spellings. | `TestTLSDialOption_UnrecognisedSwitchIsRefused`, `TestCheckInternalTLS` |
| S18.11 | `ClientTLSConfig` MUST return no configuration when the switch is off, and otherwise the same configuration `TLSDialOption` dials with, so other clients (the telemetry exporter, S19) share one posture. | `TestClientTLSConfig_OffIsNil`, `TestClientTLSConfig_TerminatedUsesSystemRootsOnly` |

## S19. Observability bootstrap

**Code.** `go/core/observx/observx.go` — `MustInit`, `Shutdown`, `Tracer`

**Purpose.** One init for the error tracker (Sentry), the OTLP trace exporter, an optional
Prometheus meter provider, and W3C propagation.

**Configuration.**

| Input | Default / effect |
|---|---|
| `ServiceName` | required; overridden by `OTEL_SERVICE_NAME` when that is non-empty |
| `OTelEndpoint` | falls back to `OTEL_EXPORTER_OTLP_ENDPOINT`; 10s construction context |
| `OTelTLS` / `W17_OTEL_TLS` | empty → follow the internal mesh: TLS exactly when a w17 client dials TLS (`W17_INTERNAL_TLS` on or `terminated`), with the same CA and client pair (system roots under `terminated`); plaintext when the switch is off. `on` / `true` / `1` / `yes` → TLS whatever the switch says. `off` / `false` / `0` / `no` → plaintext, logged when the mesh is on. Any other value → init error |
| `SampleRate` | `0` → 1.0; parent-based: a request that carries its caller's sampling decision keeps it, the rate applies to traces this service starts. Outside 0..1 or NaN → init error |
| `SentryDSN` | on when non-empty; traces sample rate fixed at 0.2 |
| `EnableMetrics` | Prometheus reader on the global meter provider |
| `ConfigAttrs` | non-secret, non-empty values → resource attribute `w17.config.<lower(key)>`; secret entries dropped |
| `W17_OBSERVX_DEBUG` | `1` / `true` / `yes` / `on` turns on `ReportEvent`; read at init only |

**Shares.** Package globals (tracer, exporter, meter provider, tracker flag, debug flag,
config, log budgets) plus the OpenTelemetry globals (tracer provider, propagator, meter
provider) and the global Sentry hub. Every component in the process sees the same state.
The export transport reads the internal-mesh variables through `grpcclient.ClientTLSConfig`
(S18).

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
| S19.10 | After `Shutdown` or a failed init, the global tracer and meter providers MUST NOT point at shut-down instances: each is reset to a no-op provider when it is still the one observx installed. | `TestShutdown_ResetsGlobalProviders`, `TestMustInit_FailureResetsGlobalProviders` |
| S19.11 | Telemetry export MUST follow the deployment's transport-security posture: TLS whenever the internal-mesh switch is on, plaintext only when the switch is off or the operator opts out explicitly. | `TestOTLPExport_FollowsInternalTLS`, `TestOTLPExport_PlaintextOnlyWhenOffOrOptedOut` (`terminated` not covered) |
| S19.12 | `OTelTLS=on` MUST force TLS with the switch off; an explicit opt-out while the mesh is on MUST be logged; an unrecognised value MUST fail init with an error naming `W17_OTEL_TLS`. | `TestOTLPExport_FollowsInternalTLS`, `TestOTLPExport_PlaintextOnlyWhenOffOrOptedOut`, `TestMustInit_RejectsUnknownOTelTLS` |
| S19.13 | Sampling MUST be parent-based: an incoming sampling decision is kept whatever the local rate. | `TestSampler_HonoursTheParentDecision` |
| S19.14 | A `SampleRate` outside 0..1, or NaN, MUST fail init instead of being clamped or ignored. | `TestMustInit_RejectsSampleRateOutsideZeroToOne` |

**Deliberate limits.**
- An exporter that is down at start is not an error: OTLP over gRPC connects lazily, and the batcher drops spans later.

## S20. Error, event and refusal reporting

**Code.** `go/core/observx/observx.go` — `ReportError`, `ReportEvent`, `ReportRefusal`, `WithRequestScope`

**Purpose.** Three channels: faults (span error plus error-tracker issue), debug events
(gated by `W17_OBSERVX_DEBUG`), and refusals (always recorded, never an issue).
`WithRequestScope` gives a request its own error-tracker hub, so breadcrumbs stay with the
request that recorded them.

**Configuration.** Log budgets, fixed: the refusal line and the error and debug stderr
fallbacks each have a budget of a burst of 50 lines, then 10 lines per second. Lines over
the budget are counted; the count is logged with the next line written on that channel and
at `Shutdown`.

**Guarantees.**

| ID | Guarantee | Test |
|---|---|---|
| S20.1 | A nil error MUST be a no-op on all three. | `TestReportError_NilNoOp` (`ReportEvent` / `ReportRefusal` not covered) |
| S20.2 | `ReportError` MUST mark the active span as failed and record the error. | `TestReportError_AttachesToActiveOTelSpan` |
| S20.3 | Without the error tracker, `ReportError` MUST log to stderr with CR/LF escaped, within the log budget (S20.8). | `TestReportError_StderrFallback` (escaping not covered) |
| S20.4 | With the error tracker, the event MUST carry service, trace_id and span_id tags. | `TestReportError_SentryPath` |
| S20.5 | `ReportEvent` MUST be a no-op unless the debug knob is on; when on it adds a span event and never sets error status. | `TestReportEvent_OffByDefault_NoOp`, `TestReportEvent_DebugKnob_StderrFallback`, `TestReportEvent_SpanEvent_NoErrorStatus`, `TestReportEvent_SentryBreadcrumb` |
| S20.6 | `ReportRefusal` MUST always record (span event, and a log line within the log budget), never set error status and never raise an issue. | `TestReportRefusal_RecordsWithoutRaising` |
| S20.7 | One request's breadcrumbs MUST NOT be attached to another request's event: breadcrumbs are recorded only on the hub the request's context carries (none without one), and `ReportError` without a request hub MUST drop the shared hub's breadcrumbs. | `TestReportError_DoesNotCarryAnotherRequestsBreadcrumbs`, `TestRequestScope_KeepsBreadcrumbsPerRequest` |
| S20.8 | Log volume from client-driven refusals, and from the error and debug fallbacks, MUST be bounded per channel; suppressed lines MUST be counted and the count logged with the next written line and at `Shutdown`. | `TestReportRefusal_LogVolumeIsBounded`, `TestShutdown_ReportsPendingSuppressedLines` |
| S20.9 | `WithRequestScope` on a context that already carries a hub MUST return it unchanged, so a nested entry point (an in-process call) stays in its caller's request. | no test |

**Deliberate limits.**
- A request whose entry point did not call `WithRequestScope` records no breadcrumbs. The SDK's entry points call it (the gRPC recovery interceptors, the REST recovery middleware, MCP tool calls); a hand-written entry point must call it too.
- Without the error tracker, `WithRequestScope` returns the context unchanged.

## S21. In-process gRPC bridge

**Code.** `go/service/inprocgrpc/inprocgrpc.go` — `New`, `Conn`, `RegisterService`, `Invoke`, `NewStream`

**Purpose.** Lets a composed binary register tier servers on a `Conn` and call them
directly, with behaviour equivalent to a wire hop.

**Configuration.**

| Option | Effect |
|---|---|
| `WithUnaryInterceptor(i)`, `WithUnaryInterceptors(...)` | append to one unary chain, in option order |
| `WithStreamInterceptor(i)`, `WithStreamInterceptors(...)` | append to one stream chain, in option order |
| `RegisterService(sd, impl)` | registers a service; registration must complete before serving (documented contract). A service registered twice, or an impl that does not satisfy the descriptor's handler type, panics, as on a wire `*grpc.Server` |

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
| S21.14 | Context errors on streams (`Header`, `SendMsg`, `RecvMsg` on both sides), and a bare context error returned by a unary or stream handler, MUST be delivered as gRPC status errors (`Canceled` / `DeadlineExceeded`), as on the wire. | `TestConn_Stream_ContextErrorsAreStatusErrors`, `TestInvoke_HandlerContextErrorIsStatusError` |
| S21.15 | Messages MUST be deep-copied, so neither side can mutate the other's value. | no direct test; error paths covered by `TestCloneMsg_NonProto`, `TestCopyProto_Errors` |
| S21.16 | A nested call MUST NOT be granted more than a wire hop. | covered by the S21.4 tests |
| S21.17 | Registering a service twice, or an impl that does not satisfy the handler type, MUST panic instead of replacing or accepting it. | `TestConn_RegisterService_DuplicateIsRefused`, `TestConn_RegisterService_WrongImplIsRefused` |
| S21.18 | Repeated interceptor options MUST append to one chain in option order; a later option MUST NOT drop an earlier one. | `TestConn_UnaryInterceptorOptionsAppend`, `TestConn_StreamInterceptorOptionsAppend` |

**Deliberate limits.**
- A stream goroutine stays alive until the caller cancels its context or reads to the end — the same contract as grpc-go.
- No message-size limit and no deadline injection: there is no wire.
