package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// Health is liveness for probes that carry no credentials: it answers
// SERVING even when the service's own interceptors refuse every call
// without a principal.
func TestHealth_IsReachableWithoutCredentials(t *testing.T) {
	noTLS(t)
	addr := freeAddr(t)
	refuseAll := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return nil, status.Error(codes.Unauthenticated, "no principal")
	}
	stop := runComponent(t, Config{
		Addr:              addr,
		RegisterServices:  func(*grpc.Server) error { return nil },
		UnaryInterceptors: []grpc.UnaryServerInterceptor{refuseAll},
	})
	defer func() { _ = stop() }()
	if resp, err := checkHealth(plainConn(t, addr)); err != nil || resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("health behind an auth interceptor = %v / %v, want SERVING without credentials", resp, err)
	}
}
