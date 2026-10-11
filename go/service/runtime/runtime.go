// Package runtime is the gRPC half of a wandering-compiler service
// binary. [GRPCComponent] packages the gRPC server lifecycle as a
// [bootstrap.Component]:
//
//   - the per-RPC OTel server span via the standard otelgrpc stats
//     handler;
//   - the caller's interceptor chain (rollback, eventbus emit, …)
//     chained after it;
//   - the internal-mesh TLS posture from the stack-wide switch
//     ([grpcserver.TLSServerOption]);
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
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/wandering-compiler/sdk/go/core/lifecycle"
	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/lib/grpcserver"
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
	Addr       string // listen address; required
	Reflection bool   // register gRPC reflection
	// ShutdownTimeout bounds the graceful stop. 0 (unset) means
	// [lifecycle.DefaultShutdownTimeout] (10s); a negative value —
	// [lifecycle.Unbounded], which is also what
	// [lifecycle.ShutdownTimeoutFromEnv] returns for an operator's
	// explicit opt-out — waits for every in-flight RPC with no bound.
	ShutdownTimeout time.Duration

	// UnaryInterceptors are chained AFTER the otelgrpc stats
	// handler — the w17 runtime interceptors (tx rollback,
	// eventbus emit) go here. Empty = none.
	//
	// DEPLOYMENT PARITY. This serves the STANDALONE deployment only: a
	// composed -server binary never builds a *grpc.Server for the tier, it
	// registers the same servers on a service/inprocgrpc.Conn, which takes
	// its chain through inprocgrpc.WithUnaryInterceptors (the generated
	// composed main passes Resources.Interceptors() to both). A tier
	// interceptor added here and nowhere else applies on the wire and
	// silently not in a composed binary — with a green build either way.
	// The same holds for a STREAM interceptor, which this Config does not
	// expose at all today: the only one installed is the recovery net
	// below, whose in-process counterpart is inlined in inprocgrpc's
	// NewStream. Whoever gives this Config a StreamInterceptors field must
	// hand the same chain to inprocgrpc.WithStreamInterceptors.
	UnaryInterceptors []grpc.UnaryServerInterceptor

	// ServerOptions is an escape hatch for additional raw
	// grpc.ServerOption values (creds, keepalive, extra stats
	// handlers). Appended after the otelgrpc handler, the internal
	// TLS credentials and the chained UnaryInterceptors, so a later
	// single-valued option (keepalive, creds) overrides the default.
	//
	// Extra interceptors go in with grpc.ChainUnaryInterceptor /
	// grpc.ChainStreamInterceptor, which run inside the recovery net.
	// The singular grpc.UnaryInterceptor / grpc.StreamInterceptor slot
	// is taken by the recovery interceptor itself (grpc-go runs that
	// slot outermost); passing one here is refused at start.
	ServerOptions []grpc.ServerOption

	// RegisterServices registers the binary's gRPC services on
	// the server (storage handlers, plugin wire-up, …). Required.
	RegisterServices func(s *grpc.Server) error

	// Shutdowns run after the server stops, before observx flush
	// — drain plugin shutdown hooks, close the eventbus, etc.
	// Each is bounded by the same 30s teardown context. A hook's
	// error does not stop the later hooks; it is reported through
	// observx.ReportError (best-effort drain on the way down, but
	// never a silent one).
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
//
// The listener follows the stack-wide internal TLS switch
// ([grpcserver.TLSServerOption] over the process environment): with the
// switch on it serves TLS, and a misconfigured switch or certificate
// makes Run return the error before listening. A clean, ctx-driven stop
// returns nil.
func GRPCComponent(cfg Config) bootstrap.Component {
	return bootstrap.Func(func(ctx context.Context) error {
		opts, err := serverOptions(cfg, os.Getenv)
		if err != nil {
			return err
		}
		return server.Run(ctx, server.Config{
			Addr:             cfg.Addr,
			Reflection:       cfg.Reflection,
			HealthServer:     server.DefaultHealthServer(),
			ShutdownTimeout:  effectiveShutdownTimeout(cfg.ShutdownTimeout),
			ServerOptions:    opts,
			RegisterServices: cfg.RegisterServices,
			OnShutdown: func() {
				runShutdowns(cfg.Shutdowns)
			},
		})
	})
}

// serverOptions builds the grpc.ServerOption list for cfg, reading the
// internal TLS posture through lookup.
func serverOptions(cfg Config, lookup grpcserver.LookupFunc) ([]grpc.ServerOption, error) {
	tlsOpt, tlsOn, err := grpcserver.TLSServerOption(lookup)
	if err != nil {
		return nil, fmt.Errorf("runtime: internal TLS: %w", err)
	}

	opts := make([]grpc.ServerOption, 0, len(cfg.ServerOptions)+8)
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
	// Internal-mesh TLS from the same switch every w17 dialer reads, so
	// the front door cannot listen in plaintext while its peers dial TLS.
	// Before cfg.ServerOptions, like keepalive: a caller that already
	// appends the same option (the documented bootstrap pattern) or its
	// own credentials still has the last word.
	if tlsOn {
		opts = append(opts, tlsOpt)
	}
	// Recovery is the OUTERMOST interceptor so it catches a panic in any
	// downstream interceptor or handler — including servers not produced
	// by grpcgen (the rpc gateway) that carry no per-handler recover. It
	// takes grpc-go's singular interceptor slot, which grpc-go always
	// runs ahead of every chained interceptor: a caller's
	// grpc.UnaryInterceptor in cfg.ServerOptions would otherwise claim
	// that slot and run outside the net. grpc-go refuses a second
	// singular interceptor, which server.Run returns as an error.
	opts = append(opts, grpc.UnaryInterceptor(recoveryUnaryInterceptor))
	opts = append(opts, grpc.StreamInterceptor(recoveryStreamInterceptor))
	if len(cfg.UnaryInterceptors) > 0 {
		opts = append(opts, grpc.ChainUnaryInterceptor(cfg.UnaryInterceptors...))
	}
	opts = append(opts, cfg.ServerOptions...)
	return opts, nil
}

// effectiveShutdownTimeout maps [Config.ShutdownTimeout] onto the bound
// [lifecycle.StopGRPC] takes: 0 (unset) is the default, a negative value
// (the unbounded opt-out) passes through, since StopGRPC reads <= 0 as
// unbounded.
func effectiveShutdownTimeout(d time.Duration) time.Duration {
	if d == 0 {
		return lifecycle.DefaultShutdownTimeout
	}
	return d
}

// shutdownDrainTimeout bounds the whole cfg.Shutdowns phase.
const shutdownDrainTimeout = 30 * time.Second

// runShutdowns runs every hook in order on one bounded context, skipping
// nil entries. A failing hook does not stop the rest; its error is
// reported, because a failed pool close or bus flush (lost messages) is
// exactly what an operator needs to see.
func runShutdowns(hooks []func(context.Context) error) {
	if len(hooks) == 0 {
		return
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	for i, fn := range hooks {
		if fn == nil {
			continue
		}
		if err := fn(drainCtx); err != nil {
			observx.ReportError(drainCtx, fmt.Errorf("runtime: shutdown hook %d: %w", i, err))
		}
	}
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
