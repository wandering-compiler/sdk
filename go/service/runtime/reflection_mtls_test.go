package runtime

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectionv1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
	"github.com/wandering-compiler/sdk/go/lib/grpcserver"
	"github.com/wandering-compiler/sdk/go/tooling/certgen"
)

// Mutual TLS authenticates the peer; it does not authorize reflection. A
// client holding a valid mesh certificate is served health, and still gets
// PermissionDenied from both reflection versions when the guard refuses.
func TestReflection_UnderMutualTLSIsStillGuarded(t *testing.T) {
	dir := t.TempDir()
	if _, err := certgen.EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(grpcclient.EnvInternalTLS, "on")
	t.Setenv(grpcclient.EnvInternalTLSCert, filepath.Join(dir, "server.crt"))
	t.Setenv(grpcclient.EnvInternalTLSKey, filepath.Join(dir, "server.key"))
	t.Setenv(grpcclient.EnvInternalTLSCA, filepath.Join(dir, "ca.crt"))
	t.Setenv(grpcserver.EnvInternalTLSClientCA, filepath.Join(dir, "ca.crt"))

	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	stop := runComponent(t, Config{
		Addr:             addr,
		RegisterServices: func(*grpc.Server) error { return nil },
		Reflection:       true,
		ReflectionAuth: func(context.Context) error {
			return status.Error(codes.PermissionDenied, "reflection is for operators")
		},
	})
	defer func() { _ = stop() }()

	conn, err := grpcclient.Dial("127.0.0.1:"+port, nil, grpc.WithAuthority("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if resp, err := checkHealth(conn); err != nil || resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("health over mutual TLS = %v / %v, want SERVING", resp, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s1, err := reflectionv1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err == nil {
		_ = s1.Send(&reflectionv1.ServerReflectionRequest{MessageRequest: &reflectionv1.ServerReflectionRequest_ListServices{}})
		_, err = s1.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("reflection v1 over mutual TLS = %v, want PermissionDenied", err)
	}
	s2, err := reflectionv1alpha.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err == nil {
		_ = s2.Send(&reflectionv1alpha.ServerReflectionRequest{MessageRequest: &reflectionv1alpha.ServerReflectionRequest_ListServices{}}) //nolint:staticcheck // SA1019: the deprecated v1alpha service is the one under test
		_, err = s2.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("reflection v1alpha over mutual TLS = %v, want PermissionDenied", err)
	}
}
