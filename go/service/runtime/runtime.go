// Package runtime is the gRPC half of a wandering-compiler service
// binary. [GRPCComponent] packages the gRPC server lifecycle as a
// [bootstrap.Component]:
//
//   - the per-RPC OTel server span via the standard otelgrpc stats
//     handler;
//   - the caller's interceptor chain (rollback, eventbus emit, …)
//     chained after it;
//   - the gRPC server lifecycle (listen / reflection / health /
//     ctx-driven graceful stop) via the embedded [server] package;
//   - teardown: the caller's shutdown hooks (plugin drains, bus
//     close), bounded.
//
// Observability + OS-signal handling are NOT here — the supervisor
// [bootstrap.Run] owns them once for the whole process (so the
// component composes with others in a single binary). [Serve] is a
// convenience that runs one GRPCComponent under that supervisor for
// a standalone gRPC binary.
//
// The binary keeps ownership of its resource wiring (DB pools, tx
// registry, eventbus dispatcher are constructed from env in main
// and captured by the RegisterServices closure); runtime owns only
// the gRPC server lifecycle. That split is deliberate — don't fold
// env-driven resource construction in here.
package runtime

import (
	"context"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
	"github.com/wandering-compiler/sdk/go/service/internal/server"
)

// Config is the input to [Serve]. The observability fields map
// straight onto [observx.Config]; the rest configure the gRPC
// server lifecycle.
type Config struct {
	// --- observability (→ observx.Config) ---
	ServiceName    string  // required; lowercase kebab-case (e.g. "app-storage")
	ServiceVersion string  // build id / sha; optional
	Environment    string  // prod / staging / dev; optional
	SentryDSN      string  // empty = Sentry off
	OTelEndpoint   string  // empty = OTLP trace export off
	SampleRate     float64 // 0 → 100% when OTel on
	EnableMetrics  bool    // Prometheus MeterProvider (gateways)

	// --- server ---
	Addr            string        // listen address; required
	Reflection      bool          // register gRPC reflection
	ShutdownTimeout time.Duration // graceful-stop bound; 0 → 10s

	// UnaryInterceptors are chained AFTER the otelgrpc stats
	// handler — the w17 runtime interceptors (tx rollback,
	// eventbus emit) go here. Empty = none.
	UnaryInterceptors []grpc.UnaryServerInterceptor

	// ServerOptions is an escape hatch for additional raw
	// grpc.ServerOption values (creds, keepalive, extra stats
	// handlers). Appended after the otelgrpc handler + the
	// chained UnaryInterceptors.
	ServerOptions []grpc.ServerOption

	// RegisterServices registers the binary's gRPC services on
	// the server (storage handlers, plugin wire-up, …). Required.
	RegisterServices func(s *grpc.Server) error

	// Shutdowns run after the server stops, before observx flush
	// — drain plugin shutdown hooks, close the eventbus, etc.
	// Each is bounded by the same 30s teardown context. Errors
	// are ignored (best-effort drain on the way down).
	Shutdowns []func(context.Context) error
}

// ObservxConfig projects the observability fields of cfg onto an
// [observx.Config] — the input the supervisor ([bootstrap.Run])
// initialises once for the whole process. A caller composing this
// gRPC component with others passes this to the supervisor.
func (cfg Config) ObservxConfig() observx.Config {
	return observx.Config{
		ServiceName:    cfg.ServiceName,
		ServiceVersion: cfg.ServiceVersion,
		Environment:    cfg.Environment,
		SentryDSN:      cfg.SentryDSN,
		OTelEndpoint:   cfg.OTelEndpoint,
		SampleRate:     cfg.SampleRate,
		EnableMetrics:  cfg.EnableMetrics,
	}
}

// GRPCComponent wraps the gRPC server lifecycle as a
// [bootstrap.Component]: builds the server with the otelgrpc span
// handler + the caller's interceptors, serves until its context is
// cancelled, then drains cfg.Shutdowns. Observability is NOT
// initialised here — the supervisor owns it (see [Config.ObservxConfig]).
// Use this to compose a gRPC binary alongside other components in a
// single process; for a standalone gRPC binary, [Serve] wraps it.
func GRPCComponent(cfg Config) bootstrap.Component {
	return bootstrap.Func(func(ctx context.Context) error {
		opts := make([]grpc.ServerOption, 0, len(cfg.ServerOptions)+5)
		// otelgrpc stats handler creates the per-RPC server span that
		// observx reads in error paths + business code reads via
		// trace.SpanFromContext.
		opts = append(opts, grpc.StatsHandler(otelgrpc.NewServerHandler()))
		// Keepalive — the SERVER half of the paired posture set by the
		// w17 clients (lib/grpcclient + toolkit/grpcx dial with Time=30s,
		// PermitWithoutStream=true). EnforcementPolicy PERMITS those pings;
		// without it the server answers an idle keepalive ping with GOAWAY
		// "too_many_pings" and tears down the very connection keepalive is
		// meant to preserve. MinTime (10s) sits safely below the client
		// Time (30s); PermitWithoutStream mirrors the client. ServerParameters
		// add a server-initiated ping so a dead client is reaped too.
		// Placed before cfg.ServerOptions so a caller can still override.
		opts = append(opts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}))
		opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}))
		// Recovery is the OUTERMOST interceptor (prepended to the
		// chain) so it catches a panic in any downstream interceptor
		// or handler — including servers not produced by grpcgen (the
		// rpc gateway) that carry no per-handler recover. A process-
		// wide net in one place instead of per-handler-only.
		unary := append([]grpc.UnaryServerInterceptor{recoveryUnaryInterceptor}, cfg.UnaryInterceptors...)
		opts = append(opts, grpc.ChainUnaryInterceptor(unary...))
		opts = append(opts, grpc.ChainStreamInterceptor(recoveryStreamInterceptor))
		opts = append(opts, cfg.ServerOptions...)

		shutdownTimeout := cfg.ShutdownTimeout
		if shutdownTimeout == 0 {
			shutdownTimeout = 10 * time.Second
		}

		return server.Run(ctx, server.Config{
			Addr:             cfg.Addr,
			Reflection:       cfg.Reflection,
			HealthServer:     server.DefaultHealthServer(),
			ShutdownTimeout:  shutdownTimeout,
			ServerOptions:    opts,
			RegisterServices: cfg.RegisterServices,
			OnShutdown: func() {
				if len(cfg.Shutdowns) == 0 {
					return
				}
				drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				for _, fn := range cfg.Shutdowns {
					if fn != nil {
						_ = fn(drainCtx)
					}
				}
			},
		})
	})
}

// Serve is the convenience entrypoint for a STANDALONE gRPC binary:
// it runs a single [GRPCComponent] under the [bootstrap.Run]
// supervisor (observability init + signal handling + graceful
// teardown). Equivalent to
// `bootstrap.Run(ctx, cfg.ObservxConfig(), GRPCComponent(cfg))`.
// Binaries that compose multiple components call bootstrap.Run
// directly with GRPCComponent(cfg) plus the others.
func Serve(ctx context.Context, cfg Config) error {
	return bootstrap.Run(ctx, cfg.ObservxConfig(), GRPCComponent(cfg))
}
