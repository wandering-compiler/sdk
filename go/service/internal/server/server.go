// Package server is the wandering-compiler's gRPC server
// lifecycle helper — listen, register services, optional
// reflection + health, then block until the context is cancelled
// and stop gracefully. Signal handling lives one level up in the
// supervisor ([bootstrap.Run]), which cancels the context; this
// helper is purely ctx-driven.
//
// It is the w17-owned replacement for the generic gox/grpcx/server
// the generated bundles used to import: same Config shape (so the
// migration is a package-path swap), trimmed to exactly what w17
// needs. Observability (Sentry + OTel) is NOT wired here — that's
// composed one level up in [runtime.Serve], which passes the
// otelgrpc stats handler + interceptor chain in via ServerOptions.
// Use this package directly when you want the bare lifecycle
// without the observability bootstrap.
package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/lifecycle"
)

// DefaultHealthServer returns a health.Server with the root
// service ("") set to SERVING. The caller may further configure
// per-service statuses before handing it to [Config].
func DefaultHealthServer() *health.Server {
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	return hs
}

// Config holds the configuration for a gRPC server.
type Config struct {
	// Addr is the TCP address to listen on (e.g. ":50051").
	Addr string

	// Reflection enables the gRPC server reflection service, which
	// lists every service and message schema the server carries. The
	// service-level authentication of the generated handlers does not
	// cover it, so it is refused at start unless ReflectionAuth guards
	// it or ReflectionUnauthenticated states that anyone who reaches
	// the port may enumerate the API.
	Reflection bool

	// ReflectionAuth, when set, runs on every reflection stream before
	// it is served; a non-nil error refuses the stream (a gRPC status
	// error is returned as is, anything else as Unauthenticated).
	ReflectionAuth func(ctx context.Context) error

	// ReflectionUnauthenticated serves reflection without
	// authentication. Only for a listener that untrusted callers cannot
	// reach (local development); logged as a warning at start.
	ReflectionUnauthenticated bool

	// HealthServer, when set, is registered on the gRPC server.
	// The caller retains full control over service statuses.
	// nil = no health service.
	HealthServer *health.Server

	// ShutdownTimeout bounds graceful shutdown — the max time to
	// wait for in-flight RPCs after a stop signal. Zero blocks
	// indefinitely (GracefulStop); after the timeout the server
	// is forcefully stopped.
	ShutdownTimeout time.Duration

	// ServerOptions are passed verbatim to grpc.NewServer — the
	// seam through which [runtime.Serve] injects the otelgrpc
	// stats handler + the w17 interceptor chain (rollback, emit).
	ServerOptions []grpc.ServerOption

	// RegisterServices registers gRPC services on the server.
	// Required.
	RegisterServices func(s *grpc.Server) error

	// OnShutdown runs after the server has stopped. May be nil.
	OnShutdown func()
}

// Run starts a gRPC server and blocks until ctx is cancelled (or
// the listener fails), then stops it gracefully (subject to
// ShutdownTimeout) and calls OnShutdown if set.
//
// A ctx-driven stop is the normal way down and returns nil; only a
// failure (validation, listen, registration, or Serve ending on its
// own) returns an error. When shutdown begins the health server, if
// any, reports NOT_SERVING for every service before the drain starts,
// so a client watching health sees the stop coming instead of SERVING
// until the connection drops. On every path that reaches OnShutdown
// the server has been stopped first, so no handler is still running
// on a connection when the hooks close the resources it uses.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Addr == "" {
		return fmt.Errorf("server: address is required")
	}
	if cfg.RegisterServices == nil {
		return fmt.Errorf("server: RegisterServices is required")
	}
	if cfg.Reflection && cfg.ReflectionAuth == nil && !cfg.ReflectionUnauthenticated {
		return fmt.Errorf("server: Reflection lists every service and message schema without authentication; set ReflectionAuth to guard it, or ReflectionUnauthenticated to serve it to anyone who reaches %s", cfg.Addr)
	}

	listener, err := listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", cfg.Addr, err)
	}

	srv, err := newServer(cfg.ServerOptions)
	if err != nil {
		_ = listener.Close()
		return err
	}

	if err = cfg.RegisterServices(srv); err != nil {
		_ = listener.Close()
		return fmt.Errorf("server: register services: %w", err)
	}

	if cfg.Reflection {
		if cfg.ReflectionAuth == nil {
			log.Printf("server: gRPC reflection on %s is served WITHOUT authentication (ReflectionUnauthenticated)", cfg.Addr)
			reflection.Register(srv)
		} else {
			reflection.Register(guardedRegistrar{srv: srv, auth: cfg.ReflectionAuth})
		}
	}
	if cfg.HealthServer != nil {
		grpc_health_v1.RegisterHealthServer(srv, cfg.HealthServer)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	// Pure ctx-driven lifecycle: the supervisor ([bootstrap.Run])
	// owns the single OS-signal handler and cancels ctx on
	// SIGINT/SIGTERM — this server only watches ctx + its own
	// listener error.
	select {
	case err = <-serveErr:
		// Serve ended on its own (e.g. the listener failed). Connections
		// it already accepted keep running handlers until the server is
		// stopped, so stop it before OnShutdown closes what they use.
		markNotServing(cfg.HealthServer)
		srv.Stop()
		if err != nil {
			err = fmt.Errorf("server: serve on %s: %w", cfg.Addr, err)
		}
	case <-ctx.Done():
		markNotServing(cfg.HealthServer)
		gracefulStop(srv, cfg.ShutdownTimeout)
		err = nil
	}

	if cfg.OnShutdown != nil {
		cfg.OnShutdown()
	}
	return err
}

// listen is the listener constructor; a test replaces it to make Serve
// fail on its own.
var listen = net.Listen

// newServer builds the grpc.Server. grpc-go panics on a conflicting
// option set (a singular grpc.UnaryInterceptor or grpc.StreamInterceptor
// given twice); that is a configuration error, so it is returned as one
// instead of taking the process down from inside the lifecycle.
func newServer(opts []grpc.ServerOption) (srv *grpc.Server, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("server: invalid server options: %v (pass extra interceptors with grpc.ChainUnaryInterceptor / grpc.ChainStreamInterceptor, not the singular grpc.UnaryInterceptor / grpc.StreamInterceptor)", r)
		}
	}()
	return grpc.NewServer(opts...), nil
}

// markNotServing flips every service on hs to NOT_SERVING (and keeps it
// there) once shutdown has begun. nil-safe.
func markNotServing(hs *health.Server) {
	if hs != nil {
		hs.Shutdown()
	}
}

// gracefulStop attempts a graceful shutdown within timeout; zero
// blocks indefinitely, otherwise a hard Stop kicks in at timeout.
//
// The bound itself lives in [lifecycle.StopGRPC] — the same
// implementation the generated rpc transport uses, so the two paths
// can't drift the way they did when this one was the only one that
// had a fallback at all.
func gracefulStop(srv *grpc.Server, timeout time.Duration) {
	lifecycle.StopGRPC(srv, timeout)
}

// guardedRegistrar registers services with auth run ahead of every
// handler. It is what reflection is registered through when
// [Config.ReflectionAuth] is set, so both reflection versions are guarded.
type guardedRegistrar struct {
	srv  *grpc.Server
	auth func(context.Context) error
}

func (g guardedRegistrar) GetServiceInfo() map[string]grpc.ServiceInfo {
	return g.srv.GetServiceInfo()
}

func (g guardedRegistrar) RegisterService(desc *grpc.ServiceDesc, impl any) {
	guarded := *desc
	guarded.Methods = make([]grpc.MethodDesc, len(desc.Methods))
	for i, m := range desc.Methods {
		h := m.Handler
		m.Handler = func(srv any, ctx context.Context, dec func(any) error, icpt grpc.UnaryServerInterceptor) (any, error) {
			if err := g.check(ctx); err != nil {
				return nil, err
			}
			return h(srv, ctx, dec, icpt)
		}
		guarded.Methods[i] = m
	}
	guarded.Streams = make([]grpc.StreamDesc, len(desc.Streams))
	for i, sd := range desc.Streams {
		h := sd.Handler
		sd.Handler = func(srv any, stream grpc.ServerStream) error {
			if err := g.check(stream.Context()); err != nil {
				return err
			}
			return h(srv, stream)
		}
		guarded.Streams[i] = sd
	}
	g.srv.RegisterService(&guarded, impl)
}

// check runs auth and maps a refusal onto a gRPC status. A plain error's
// text is not sent to the caller.
func (g guardedRegistrar) check(ctx context.Context) error {
	err := g.auth(ctx)
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok {
		return st.Err()
	}
	return status.Error(codes.Unauthenticated, "reflection: unauthenticated")
}
