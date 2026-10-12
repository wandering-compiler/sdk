package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
	"github.com/wandering-compiler/sdk/go/tooling/certgen"
)

// ping calls the ordinary test method over conn.
func ping(conn *grpc.ClientConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return conn.Invoke(ctx, "/test.Ping/Ping", &emptypb.Empty{}, &emptypb.Empty{})
}

// Every RPC gets a server span from the stats handler — also one an
// interceptor refuses before the handler runs.
func TestServerOptions_EveryRPCHasAServerSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	refuse := func(context.Context, any, *grpc.UnaryServerInfo, grpc.UnaryHandler) (any, error) {
		return nil, status.Error(codes.PermissionDenied, "no")
	}
	conn := serveWithOptions(t, Config{UnaryInterceptors: []grpc.UnaryServerInterceptor{refuse}}, nil)
	if err := ping(conn); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("call = %v, want the interceptor's refusal", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, s := range rec.Ended() {
			if s.SpanKind() == trace.SpanKindServer && strings.Contains(s.Name(), "test.Ping/Ping") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no server span for the refused call; spans = %d", len(rec.Ended()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cfg.UnaryInterceptors run in slice order, the first outermost.
func TestServerOptions_UnaryInterceptorsRunInSliceOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	mark := func(name string) grpc.UnaryServerInterceptor {
		return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			mu.Lock()
			order = append(order, name+">")
			mu.Unlock()
			resp, err := h(ctx, req)
			mu.Lock()
			order = append(order, "<"+name)
			mu.Unlock()
			return resp, err
		}
	}
	conn := serveWithOptions(t, Config{UnaryInterceptors: []grpc.UnaryServerInterceptor{mark("a"), mark("b"), mark("c")}}, nil)
	if err := ping(conn); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, " "); got != "a> b> c> <c <b <a" {
		t.Errorf("interceptor order = %q, want a outermost, then b, then c", got)
	}
}

// Credentials in cfg.ServerOptions win over the internal TLS credentials:
// they come later in the option list, so a caller's own certificate is the
// one served.
func TestServerOptions_CallerCredentialsOverrideInternalTLS(t *testing.T) {
	internal, own := t.TempDir(), t.TempDir()
	for _, dir := range []string{internal, own} {
		if _, err := certgen.EnsureCerts(dir, []string{"localhost"}); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		grpcclient.EnvInternalTLS:     "on",
		grpcclient.EnvInternalTLSCert: filepath.Join(internal, "server.crt"),
		grpcclient.EnvInternalTLSKey:  filepath.Join(internal, "server.key"),
		grpcclient.EnvInternalTLSCA:   filepath.Join(internal, "ca.crt"),
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(own, "server.crt"), filepath.Join(own, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	opts, err := serverOptions(Config{ServerOptions: []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})),
	}}, func(k string) string { return env[k] }, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(opts...)
	srv.RegisterService(&pingDesc, struct{}{})
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dial := func(caDir string) error {
		pem, err := os.ReadFile(filepath.Join(caDir, "ca.crt"))
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})))
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return ping(conn)
	}
	if err := dial(own); err != nil {
		t.Errorf("a client trusting the caller's CA = %v, want served (caller credentials win)", err)
	}
	if err := dial(internal); err == nil {
		t.Error("a client trusting only the internal CA was served; the internal credentials won over the caller's")
	}
}

// A panic is reported with the method, the value and the stack — on the
// operator's channel, while the client gets only a generic Internal.
func TestRecoveryInterceptors_ReportValueStackAndMethod(t *testing.T) {
	buf := captureLog(t)
	_, err := recoveryUnaryInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/svc.Orders/Place"},
		func(context.Context, any) (any, error) { panic("ledger nil") })
	if status.Code(err) != codes.Internal {
		t.Fatalf("err = %v, want Internal", err)
	}
	out := buf.String()
	for _, want := range []string{"/svc.Orders/Place", "ledger nil", "goroutine "} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q; log = %q", want, out)
		}
	}

	buf.Reset()
	err = recoveryStreamInterceptor(nil, fakeStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/svc.Orders/Watch"},
		func(any, grpc.ServerStream) error { panic("cursor closed") })
	if status.Code(err) != codes.Internal {
		t.Fatalf("stream err = %v, want Internal", err)
	}
	out = buf.String()
	for _, want := range []string{"/svc.Orders/Watch", "cursor closed", "goroutine "} {
		if !strings.Contains(out, want) {
			t.Errorf("stream report lacks %q; log = %q", want, out)
		}
	}
}
