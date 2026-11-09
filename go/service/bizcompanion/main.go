package bizcompanion

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
	"github.com/wandering-compiler/sdk/go/service/healthcheck"
	"github.com/wandering-compiler/sdk/go/service/runtime"
	"github.com/wandering-compiler/sdk/go/service/secret"
)

// ManifestEnv names the business bundle's manifest the standalone companion
// serves (its image copies the manifest and the descriptor set beside each
// other).
const ManifestEnv = "W17_COMPANION_MANIFEST"

// DefaultListenAddr is where the standalone companion listens when
// [ListenEnv] is unset.
const DefaultListenAddr = ":50051"

// ShutdownTimeout bounds the drain of in-flight calls on a stop.
const ShutdownTimeout = 10 * time.Second

// ListenEnv is the variable the standalone companion reads its listen
// address from: <PREFIX>_BUSINESS_COMPANION_LISTEN_ADDR.
func ListenEnv(cfg *Config) string { return cfg.EnvPrefix + "_BUSINESS_COMPANION_LISTEN_ADDR" }

// ListenAddr is the address the standalone companion serves on: [ListenEnv],
// else [DefaultListenAddr]. One function for the server and for
// `<binary> health`, so the probe asks the port the server bound.
func ListenAddr(cfg *Config, getenv func(string) string) string {
	if addr := getenv(ListenEnv(cfg)); addr != "" {
		return addr
	}
	return DefaultListenAddr
}

// ServiceName is the companion's name in telemetry and in the deployment:
// <domain>-business-companion.
func ServiceName(cfg *Config) string { return cfg.Domain + "-business-companion" }

// Main is the whole `main` of the split companion bundle: it serves the
// manifest [ManifestEnv] names until SIGINT/SIGTERM. `<binary> health`
// probes the listener (the image's HEALTHCHECK) instead.
//
// The server is the w17 gRPC runtime's (service/runtime): internal TLS per
// W17_INTERNAL_TLS, grpc.health.v1, the per-RPC span, recovery, OTel and
// Sentry from W17_OTEL_ENDPOINT / W17_SENTRY_DSN, and a drain of
// [ShutdownTimeout].
func Main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv); err != nil {
		log.Fatalf("business companion: %v", err)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	path := getenv(ManifestEnv)
	if path == "" {
		return errors.New(ManifestEnv + " is required: the business bundle's w17-business.json")
	}
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	addr := ListenAddr(cfg, getenv)
	if handled, err := healthcheck.Dispatch(ctx, args, healthcheck.Options{
		Probes: []healthcheck.Probe{healthcheck.GRPC("grpc", addr)},
		Getenv: getenv,
		Final:  true,
	}); handled {
		return err
	}
	if cfg.Companion == nil {
		return fmt.Errorf("%s has no companion section: the %s business tier needs no companion", path, cfg.Domain)
	}
	return bootstrap.Run(ctx, observx.Config{
		ServiceName:    ServiceName(cfg),
		ServiceVersion: getenv("W17_SERVICE_VERSION"),
		Environment:    getenv("W17_ENV"),
		SentryDSN:      getenv("W17_SENTRY_DSN"),
		OTelEndpoint:   getenv("W17_OTEL_ENDPOINT"),
	}, Component(cfg, addr))
}

// Component is the companion's gRPC server on addr as a bootstrap
// component: what [Main] runs, for a binary that composes it with others
// under one supervisor.
func Component(cfg *Config, addr string) bootstrap.Component {
	return bootstrap.Func(func(ctx context.Context) error {
		secrets, err := secret.NewResolver()
		if err != nil {
			return fmt.Errorf("secrets: %w", err)
		}
		var closer func(context.Context) error
		log.Printf("%s listening on %s", ServiceName(cfg), addr)
		return runtime.GRPCComponent(runtime.Config{
			ServiceName:     ServiceName(cfg),
			Addr:            addr,
			ShutdownTimeout: ShutdownTimeout,
			RegisterServices: func(s *grpc.Server) error {
				c, err := Register(ctx, s, cfg, secrets.Lookup)
				closer = c
				return err
			},
			Shutdowns: []func(context.Context) error{func(ctx context.Context) error {
				if closer == nil {
					return nil
				}
				return closer(ctx)
			}},
		}).Run(ctx)
	})
}
