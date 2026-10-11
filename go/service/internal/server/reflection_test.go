package server

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectionv1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
)

// runOnLoopback starts Run with cfg and returns the bound address.
func runOnLoopback(t *testing.T, cfg Config) string {
	t.Helper()
	addrCh := make(chan string, 1)
	prev := listen
	listen = func(network, addr string) (net.Listener, error) {
		l, err := net.Listen(network, addr)
		if err == nil {
			addrCh <- l.Addr().String()
		}
		return l, err
	}
	t.Cleanup(func() { listen = prev })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg.Addr = "127.0.0.1:0"
	if cfg.RegisterServices == nil {
		cfg.RegisterServices = func(*grpc.Server) error { return nil }
	}
	go func() { done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case a := <-addrCh:
		return a
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not start")
	}
	return ""
}

// listServices asks both reflection versions for the service list.
func listServices(t *testing.T, addr string, md metadata.MD) (v1, v1alpha error) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, md)

	s1, err := reflectionv1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err == nil {
		// Send reports only that the stream ended (io.EOF); the status
		// comes from Recv.
		_ = s1.Send(&reflectionv1.ServerReflectionRequest{MessageRequest: &reflectionv1.ServerReflectionRequest_ListServices{}})
		_, err = s1.Recv()
	}
	v1 = err

	s2, err := reflectionv1alpha.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err == nil {
		_ = s2.Send(&reflectionv1alpha.ServerReflectionRequest{MessageRequest: &reflectionv1alpha.ServerReflectionRequest_ListServices{}})
		_, err = s2.Recv()
	}
	v1alpha = err
	return v1, v1alpha
}

// Reflection with nothing guarding it, and no statement that it may be
// served unauthenticated, is refused at start rather than listing the API
// to anyone who reaches the port.
func TestRun_UnguardedReflectionIsRefused(t *testing.T) {
	err := Run(context.Background(), Config{
		Addr:             "127.0.0.1:0",
		Reflection:       true,
		RegisterServices: func(*grpc.Server) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ReflectionAuth") || !strings.Contains(err.Error(), "ReflectionUnauthenticated") {
		t.Fatalf("Run = %v, want a start error naming ReflectionAuth and ReflectionUnauthenticated", err)
	}
}

// With ReflectionAuth set, both reflection versions answer only callers the
// guard accepts; a plain error from the guard is not echoed to the caller.
func TestRun_ReflectionAuthGuardsBothVersions(t *testing.T) {
	addr := runOnLoopback(t, Config{
		Reflection: true,
		ReflectionAuth: func(ctx context.Context) error {
			md, _ := metadata.FromIncomingContext(ctx)
			if v := md.Get("authorization"); len(v) == 1 && v[0] == "Bearer ok" {
				return nil
			}
			return errBadToken
		},
	})

	v1, v1alpha := listServices(t, addr, nil)
	for name, err := range map[string]error{"v1": v1, "v1alpha": v1alpha} {
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s without credentials: %v, want Unauthenticated", name, err)
		}
		if err != nil && strings.Contains(err.Error(), errBadToken.Error()) {
			t.Errorf("%s: the guard's own error text reached the caller: %v", name, err)
		}
	}
	v1, v1alpha = listServices(t, addr, metadata.Pairs("authorization", "Bearer ok"))
	if v1 != nil || v1alpha != nil {
		t.Errorf("with credentials: v1=%v v1alpha=%v, want both served", v1, v1alpha)
	}
}

var errBadToken = errorString("token rejected by the auth backend")

type errorString string

func (e errorString) Error() string { return string(e) }

// A guard that returns a gRPC status has that status passed through.
func TestRun_ReflectionAuthStatusPassesThrough(t *testing.T) {
	addr := runOnLoopback(t, Config{
		Reflection:     true,
		ReflectionAuth: func(context.Context) error { return status.Error(codes.PermissionDenied, "no") },
	})
	if v1, _ := listServices(t, addr, nil); status.Code(v1) != codes.PermissionDenied {
		t.Errorf("v1: %v, want PermissionDenied", v1)
	}
}

// The explicit unauthenticated opt-in serves reflection; without the flag
// it is not registered at all.
func TestRun_ReflectionRegisteredOnlyWhenSet(t *testing.T) {
	on := runOnLoopback(t, Config{Reflection: true, ReflectionUnauthenticated: true})
	if v1, v1alpha := listServices(t, on, nil); v1 != nil || v1alpha != nil {
		t.Errorf("opted-in reflection: v1=%v v1alpha=%v", v1, v1alpha)
	}
	off := runOnLoopback(t, Config{})
	if v1, _ := listServices(t, off, nil); status.Code(v1) != codes.Unimplemented {
		t.Errorf("reflection off: %v, want Unimplemented", v1)
	}
}
