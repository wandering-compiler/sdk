package runtime

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

// denyAll is the shape of an authenticating interceptor facing a caller
// with no credentials.
func denyAllUnary(context.Context, any, *grpc.UnaryServerInfo, grpc.UnaryHandler) (any, error) {
	return nil, status.Error(codes.Unauthenticated, "no credentials")
}

func denyAllStream(any, grpc.ServerStream, *grpc.StreamServerInfo, grpc.StreamHandler) error {
	return status.Error(codes.Unauthenticated, "no credentials")
}

// pingDesc is an ordinary service the deny-all interceptors must still
// guard: the decoy that proves they are installed.
var pingDesc = grpc.ServiceDesc{
	ServiceName: "test.Ping",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Ping",
		Handler: func(srv any, ctx context.Context, dec func(any) error, icpt grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(context.Context, any) (any, error) { return &emptypb.Empty{}, nil }
			if icpt == nil {
				return h(ctx, in)
			}
			return icpt(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Ping/Ping"}, h)
		},
	}},
}

// serveWithOptions runs a server built from serverOptions(cfg) on an
// in-memory listener, registering hs as the health service.
func serveWithOptions(t *testing.T, cfg Config, hs grpc_health_v1.HealthServer) *grpc.ClientConn {
	t.Helper()
	opts, err := serverOptions(cfg, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(opts...)
	grpc_health_v1.RegisterHealthServer(srv, hs)
	srv.RegisterService(&pingDesc, struct{}{})
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return conn
}

// Probes carry no credentials, so an authenticating interceptor — passed as
// UnaryInterceptors or through ServerOptions — must not stand between them and
// the health service, while it still guards everything else.
func TestHealth_ReachableWithoutCredentialsPastCallerInterceptors(t *testing.T) {
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	conn := serveWithOptions(t, Config{
		UnaryInterceptors: []grpc.UnaryServerInterceptor{denyAllUnary},
		ServerOptions: []grpc.ServerOption{
			grpc.ChainUnaryInterceptor(denyAllUnary),
			grpc.ChainStreamInterceptor(denyAllStream),
		},
	}, hs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hc := grpc_health_v1.NewHealthClient(conn)

	resp, err := hc.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("Check = %v, %v; want SERVING", resp, err)
	}
	if _, err := hc.List(ctx, &grpc_health_v1.HealthListRequest{}); err != nil {
		t.Errorf("List: %v", err)
	}
	w, err := hc.Watch(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if first, err := w.Recv(); err != nil || first.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("Watch first = %v, %v; want SERVING", first, err)
	}
	// An unknown service is still answered by the health server itself.
	if _, err := hc.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Check(unknown) code = %v, want NotFound from the health server", status.Code(err))
	}

	// Decoy: the interceptors are in force for an ordinary method.
	if err := conn.Invoke(ctx, "/test.Ping/Ping", &emptypb.Empty{}, &emptypb.Empty{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Ping code = %v, want Unauthenticated: the caller's interceptors must still guard other methods", status.Code(err))
	}
}

// panickyHealth panics on every call; the recovery net must still catch it.
type panickyHealth struct {
	grpc_health_v1.UnimplementedHealthServer
}

func (panickyHealth) Check(context.Context, *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	panic("health exploded")
}

func (panickyHealth) Watch(*grpc_health_v1.HealthCheckRequest, grpc.ServerStreamingServer[grpc_health_v1.HealthCheckResponse]) error {
	panic("health exploded")
}

func TestHealth_StaysInsideRecovery(t *testing.T) {
	conn := serveWithOptions(t, Config{UnaryInterceptors: []grpc.UnaryServerInterceptor{denyAllUnary}}, panickyHealth{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hc := grpc_health_v1.NewHealthClient(conn)
	if _, err := hc.Check(ctx, &grpc_health_v1.HealthCheckRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("Check code = %v, want Internal from the recovery net", status.Code(err))
	}
	w, err := hc.Watch(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if _, err := w.Recv(); status.Code(err) != codes.Internal {
		t.Errorf("Watch code = %v, want Internal from the recovery net", status.Code(err))
	}
}
