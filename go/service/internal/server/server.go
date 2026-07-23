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
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
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

	// Reflection enables the gRPC server reflection service.
	Reflection bool

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
func Run(ctx context.Context, cfg Config) error {
	if cfg.Addr == "" {
		return fmt.Errorf("server: address is required")
	}
	if cfg.RegisterServices == nil {
		return fmt.Errorf("server: RegisterServices is required")
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", cfg.Addr, err)
	}

	srv := grpc.NewServer(cfg.ServerOptions...)

	if err = cfg.RegisterServices(srv); err != nil {
		_ = listener.Close()
		return fmt.Errorf("server: register services: %w", err)
	}

	if cfg.Reflection {
		reflection.Register(srv)
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
		// Server stopped on its own (e.g. listener closed).
	case <-ctx.Done():
		gracefulStop(srv, cfg.ShutdownTimeout)
		err = ctx.Err()
	}

	if cfg.OnShutdown != nil {
		cfg.OnShutdown()
	}
	return err
}

// gracefulStop attempts a graceful shutdown within timeout; zero
// blocks indefinitely, otherwise a hard Stop kicks in at timeout.
func gracefulStop(srv *grpc.Server, timeout time.Duration) {
	if timeout == 0 {
		srv.GracefulStop()
		return
	}
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		srv.Stop()
	}
}
