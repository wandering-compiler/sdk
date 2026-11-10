package grpcclient_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/core/grpcerr"
	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// unavailableOnce is a handler that applies a write on every run and then
// answers err on its first run only.
type unavailableOnce struct {
	healthpb.UnimplementedHealthServer
	err  error
	runs atomic.Int32
}

func (h *unavailableOnce) Check(context.Context, *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if h.runs.Add(1) == 1 {
		return nil, h.err
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

// serveHealth serves h on a loopback listener with opts.
func serveHealth(t *testing.T, h healthpb.HealthServer, opts ...grpc.ServerOption) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(opts...)
	healthpb.RegisterHealthServer(srv, h)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func checkOnce(t *testing.T, addr string) error {
	t.Helper()
	conn, err := grpcclient.Dial(addr, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true))
	return err
}

// An UNAVAILABLE a w17 server's handler returned is the call's answer: the
// handler ran (it may have written), so the client does not run it again.
func TestDial_AHandlersUnavailableIsNotReplayed(t *testing.T) {
	h := &unavailableOnce{err: status.Error(codes.Unavailable, "database unavailable")}
	addr := serveHealth(t, h,
		grpc.ChainUnaryInterceptor(grpcerr.RecoverUnaryInterceptor))
	err := checkOnce(t, addr)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("caller saw %v, want the handler's UNAVAILABLE", err)
	}
	if n := h.runs.Load(); n != 1 {
		t.Fatalf("one call ran the handler %d times", n)
	}
}

// A handler's answer marked replay-safe (nothing ran: the database was never
// reached) stays retryable, and so does a trailers-only UNAVAILABLE from a
// server that ran no w17 handler — a transport-level refusal.
func TestDial_AReplaySafeOrTransportUnavailableIsRetried(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		opts []grpc.ServerOption
	}{
		"replay-safe": {
			err:  grpcerr.ReplaySafe(status.Error(codes.Unavailable, "database unavailable")),
			opts: []grpc.ServerOption{grpc.ChainUnaryInterceptor(grpcerr.RecoverUnaryInterceptor)},
		},
		"no w17 handler": {err: status.Error(codes.Unavailable, "refused")},
	} {
		t.Run(name, func(t *testing.T) {
			h := &unavailableOnce{err: tc.err}
			if err := checkOnce(t, serveHealth(t, h, tc.opts...)); err != nil {
				t.Fatalf("the retry should have succeeded: %v", err)
			}
			if n := h.runs.Load(); n != 2 {
				t.Fatalf("handler ran %d times, want 2 (one retry)", n)
			}
		})
	}
}
