package runtime

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/core/lifecycle"
	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
	"github.com/wandering-compiler/sdk/go/tooling/certgen"
)

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// noTLS pins the internal TLS switch off for a test, whatever the
// environment the tests run in says.
func noTLS(t *testing.T) {
	t.Setenv(grpcclient.EnvInternalTLS, "")
}

// checkHealth calls the health service over conn until it answers or the
// deadline passes, and returns the last result.
func checkHealth(conn *grpc.ClientConn) (*grpc_health_v1.HealthCheckResponse, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		resp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		cancel()
		if err == nil || status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			return resp, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func plainConn(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// runComponent starts GRPCComponent(cfg) and returns a stop func that
// cancels it and returns its result.
func runComponent(t *testing.T, cfg Config) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- GRPCComponent(cfg).Run(ctx) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("component did not stop after ctx cancel")
			return nil
		}
	}
}

// A clean, ctx-driven stop returns nil — the Component contract — at the
// component, through Serve, and through the supervisor next to a sibling
// that also stops cleanly. A main that exits non-zero on error would
// otherwise mark every deploy and scale-down as a failure.
func TestGRPCComponent_CleanStopReturnsNil(t *testing.T) {
	noTLS(t)
	cfg := Config{
		ServiceName:      "test-runtime",
		Addr:             "127.0.0.1:0",
		RegisterServices: func(*grpc.Server) error { return nil },
	}

	stop := runComponent(t, cfg)
	time.Sleep(100 * time.Millisecond)
	if err := stop(); err != nil {
		t.Errorf("GRPCComponent.Run = %v, want nil on a clean stop", err)
	}

	for name, run := range map[string]func(context.Context) error{
		"Serve": func(ctx context.Context) error { return Serve(ctx, cfg) },
		"bootstrap.Run with a sibling": func(ctx context.Context) error {
			sibling := bootstrap.Func(func(ctx context.Context) error { <-ctx.Done(); return nil })
			return bootstrap.Run(ctx, cfg.ObservxConfig(), GRPCComponent(cfg), sibling)
		},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- run(ctx) }()
		time.Sleep(150 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s = %v, want nil on a clean stop", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not return", name)
		}
	}
}

// With the internal TLS switch on, the front door serves TLS (a w17 client
// dialing with the stack CA gets an answer, a plaintext client does not).
func TestGRPCComponent_ServesInternalTLS(t *testing.T) {
	dir := t.TempDir()
	if _, err := certgen.EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(grpcclient.EnvInternalTLS, "on")
	t.Setenv(grpcclient.EnvInternalTLSCert, filepath.Join(dir, "server.crt"))
	t.Setenv(grpcclient.EnvInternalTLSKey, filepath.Join(dir, "server.key"))
	t.Setenv(grpcclient.EnvInternalTLSCA, filepath.Join(dir, "ca.crt"))

	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	stop := runComponent(t, Config{Addr: addr, RegisterServices: func(*grpc.Server) error { return nil }})
	defer func() { _ = stop() }()

	// Dial the loopback address the listener is bound to, verifying the
	// leaf under the name it was issued for.
	tlsConn, err := grpcclient.Dial("127.0.0.1:"+port, nil, grpc.WithAuthority("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tlsConn.Close() }()
	if resp, err := checkHealth(tlsConn); err != nil || resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("TLS health check = %v / %v, want SERVING", resp, err)
	}

	if _, err := checkHealth(plainConn(t, addr)); err == nil {
		t.Error("a plaintext client was served while internal TLS is on")
	}
}

// A misconfigured switch fails the component at start instead of falling
// back to a plaintext listener.
func TestGRPCComponent_InternalTLSMisconfigurationFailsStart(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"on without cert/key": {grpcclient.EnvInternalTLS: "on"},
		"unrecognised value":  {grpcclient.EnvInternalTLS: "enabled"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(grpcclient.EnvInternalTLSCert, "")
			t.Setenv(grpcclient.EnvInternalTLSKey, "")
			for k, v := range env {
				t.Setenv(k, v)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := GRPCComponent(Config{
				Addr:             "127.0.0.1:0",
				RegisterServices: func(*grpc.Server) error { return nil },
			}).Run(ctx)
			if err == nil || !strings.Contains(err.Error(), grpcclient.EnvInternalTLS) {
				t.Fatalf("Run = %v, want a start-up error naming %s", err, grpcclient.EnvInternalTLS)
			}
		})
	}
}

// Every caller-supplied unary interceptor runs inside the recovery net:
// one in cfg.UnaryInterceptors, and one chained in through ServerOptions.
// A panic in either becomes Internal and the server keeps serving.
func TestGRPCComponent_RecoveryWrapsCallerInterceptors(t *testing.T) {
	noTLS(t)
	panicky := func(context.Context, any, *grpc.UnaryServerInfo, grpc.UnaryHandler) (any, error) {
		panic("interceptor boom")
	}
	for name, cfg := range map[string]Config{
		"UnaryInterceptors":                   {UnaryInterceptors: []grpc.UnaryServerInterceptor{panicky}},
		"ServerOptions ChainUnaryInterceptor": {ServerOptions: []grpc.ServerOption{grpc.ChainUnaryInterceptor(panicky)}},
	} {
		t.Run(name, func(t *testing.T) {
			addr := freeAddr(t)
			cfg.Addr = addr
			// An ordinary method: health calls leave the chain ahead of the
			// caller's interceptors (health.go), so they cannot probe them.
			cfg.RegisterServices = func(s *grpc.Server) error {
				s.RegisterService(&pingDesc, struct{}{})
				return nil
			}
			stop := runComponent(t, cfg)
			conn := plainConn(t, addr)
			for i := 0; i < 2; i++ { // the second call proves the server survived
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := conn.Invoke(ctx, "/test.Ping/Ping", &emptypb.Empty{}, &emptypb.Empty{})
				cancel()
				if status.Code(err) != codes.Internal {
					t.Fatalf("call %d = %v, want Internal from the recovery net", i, err)
				}
			}
			if err := stop(); err != nil {
				t.Errorf("stop = %v", err)
			}
		})
	}
}

// A singular grpc.UnaryInterceptor / StreamInterceptor in ServerOptions
// would run ahead of the recovery net; it is refused at start.
func TestGRPCComponent_SingularInterceptorOptionIsRefused(t *testing.T) {
	noTLS(t)
	for name, opt := range map[string]grpc.ServerOption{
		"unary": grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			return h(ctx, req)
		}),
		"stream": grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			return h(srv, ss)
		}),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := GRPCComponent(Config{
				Addr:             "127.0.0.1:0",
				ServerOptions:    []grpc.ServerOption{opt},
				RegisterServices: func(*grpc.Server) error { return nil },
			}).Run(ctx)
			// The refusal comes at start, as a configuration error — not
			// as the context's own error after serving until the deadline.
			if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "ChainUnaryInterceptor") {
				t.Fatalf("Run = %v, want a start-up refusal pointing at the chained option", err)
			}
		})
	}
}

// An operator's opt-out read from the environment stays unbounded when it
// is handed to Config.ShutdownTimeout, where 0 means the default.
func TestShutdownTimeout_EnvOptOutStaysUnbounded(t *testing.T) {
	env := map[string]string{"APP_SHUTDOWN_TIMEOUT_SECONDS": "0"}
	fromEnv := lifecycle.ShutdownTimeoutFromEnv("APP", func(k string) string { return env[k] })
	if got := effectiveShutdownTimeout(fromEnv); got > 0 {
		t.Errorf("env opt-out → bound %v, want unbounded (<= 0)", got)
	}
	if got := effectiveShutdownTimeout(0); got != lifecycle.DefaultShutdownTimeout {
		t.Errorf("unset → %v, want %v", got, lifecycle.DefaultShutdownTimeout)
	}
	if got := effectiveShutdownTimeout(3 * time.Second); got != 3*time.Second {
		t.Errorf("3s → %v", got)
	}
}

// captureLog redirects the standard logger (observx's fallback sink when
// no error tracker is configured) for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// A failing shutdown hook is reported, and later hooks still run.
func TestGRPCComponent_ShutdownHookErrorIsReported(t *testing.T) {
	noTLS(t)
	buf := captureLog(t)
	var order []int
	stop := runComponent(t, Config{
		Addr:             "127.0.0.1:0",
		RegisterServices: func(*grpc.Server) error { return nil },
		Shutdowns: []func(context.Context) error{
			func(context.Context) error { order = append(order, 0); return errors.New("bus flush lost 3 messages") },
			nil,
			func(context.Context) error { order = append(order, 2); return nil },
		},
	})
	time.Sleep(100 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatalf("stop = %v", err)
	}
	if len(order) != 2 || order[0] != 0 || order[1] != 2 {
		t.Errorf("hooks ran %v, want [0 2] in order", order)
	}
	if !strings.Contains(buf.String(), "bus flush lost 3 messages") {
		t.Errorf("the hook's error was not reported; log = %q", buf.String())
	}
}
