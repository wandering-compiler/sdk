package privatesurface_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/privatesurface"
	"github.com/wandering-compiler/sdk/go/tooling/certgen"
)

// pki mints the dev CA plus a leaf per name, returning a lookup per identity.
func pki(t *testing.T, ids map[string]string) map[string]privatesurface.LookupFunc {
	t.Helper()
	dir := t.TempDir()
	out := map[string]privatesurface.LookupFunc{}
	for name, uri := range ids {
		if _, err := certgen.EnsureIdentity(dir, name, uri, []string{"identity-private", "localhost"}); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{
			privatesurface.EnvCert: filepath.Join(dir, name+".crt"),
			privatesurface.EnvKey:  filepath.Join(dir, name+".key"),
			privatesurface.EnvCA:   filepath.Join(dir, "ca.crt"),
		}
		out[name] = func(k string) string { return env[k] }
	}
	return out
}

// A gateway behind the proxy: the caller is the identity the proxy forwards,
// accepted only from the proxy's own certificate.
func TestCallerOnlyThroughTheProxy(t *testing.T) {
	look := pki(t, map[string]string{
		"gateway": "spiffe://w17/identity/identity-gateway-private",
		"proxy":   privatesurface.DefaultProxyID,
		"caller":  "spiffe://w17/alpha/alpha-gateway",
	})
	srvTLS, err := privatesurface.ServerTLS(look["gateway"])
	if err != nil {
		t.Fatal(err)
	}
	var got privatesurface.Identity
	var gotErr error
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(srvTLS)), grpc.UnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			got, gotErr = privatesurface.Caller(ctx, privatesurface.DefaultProxyID)
			if gotErr != nil {
				return nil, gotErr
			}
			return h(ctx, req)
		}))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	call := func(who string, md metadata.MD) error {
		cliTLS, err := privatesurface.ClientTLS(look[who], "identity-private")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cliTLS)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), md), 5*time.Second)
		defer cancel()
		_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		return err
	}

	if err := call("proxy", metadata.Pairs(privatesurface.CallerMetadataKey, "spiffe://w17/alpha/alpha-gateway")); err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	if got != (privatesurface.Identity{Domain: "alpha", Bundle: "alpha-gateway"}) {
		t.Errorf("caller = %+v", got)
	}
	// A caller dialing the gateway itself, naming itself: not the proxy.
	err = call("caller", metadata.Pairs(privatesurface.CallerMetadataKey, "spiffe://w17/alpha/alpha-gateway"))
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("a caller past the proxy: %v, want Unauthenticated", err)
	}
	// The proxy forwarding nothing.
	if err := call("proxy", nil); status.Code(err) != codes.Unauthenticated {
		t.Errorf("no forwarded caller: %v, want Unauthenticated", err)
	}
	// No client certificate at all: the handshake ends it.
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tlsNoCert)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Error("a client without a certificate was served")
	}
	_ = conn.Close()
}

func TestAdmitAndParse(t *testing.T) {
	id, err := privatesurface.ParseIdentity("spiffe://w17/alpha/alpha-admin")
	if err != nil || id.Domain != "alpha" || id.Bundle != "alpha-admin" || id.URI() != "spiffe://w17/alpha/alpha-admin" {
		t.Fatalf("parse: %+v %v", id, err)
	}
	for _, bad := range []string{"spiffe://w17/alpha", "spiffe://other/a/b", "https://w17/a/b", "spiffe://w17/a/b/c", ""} {
		if _, err := privatesurface.ParseIdentity(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if err := privatesurface.Admit(id, map[string]bool{"alpha": true}); err != nil {
		t.Error(err)
	}
	if err := privatesurface.Admit(id, map[string]bool{"beta": true}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a domain not admitted: %v", err)
	}
}

func TestMaterialIsRequired(t *testing.T) {
	if _, err := privatesurface.ServerTLS(func(string) string { return "" }); err == nil {
		t.Error("a gateway without its leaf and CA would serve")
	}
	if _, err := privatesurface.ClientTLS(func(string) string { return "" }, "x"); err == nil {
		t.Error("a caller without its leaf and CA would dial")
	}
}

var tlsNoCert = tlsConfigInsecure()

// The private gateway passes on the request facts a calling gateway states,
// never an identity stamp, and a product asks only for itself.
func TestForwarded(t *testing.T) {
	alpha := privatesurface.Identity{Domain: "alpha", Bundle: "alpha-admin"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-w17-dpop-jkt", "jkt1",
		"x-w17-client-addr", "10.0.0.7",
		"x-w17-product", "alpha",
		"x-w17-user", `{"user_id":"forged"}`,
		"x-w17-scope-org_id", "o1",
		"w17-label-tenant", "t",
		"x-w17-caller-domain", "beta",
		"authorization", "Bearer x",
	))
	md, err := privatesurface.Forwarded(ctx, alpha)
	if err != nil {
		t.Fatal(err)
	}
	if md.Get("x-w17-dpop-jkt")[0] != "jkt1" || md.Get("x-w17-client-addr")[0] != "10.0.0.7" || md.Get("x-w17-product")[0] != "alpha" {
		t.Errorf("forwarded %v", md)
	}
	for _, k := range []string{"x-w17-user", "x-w17-scope-org_id", "w17-label-tenant", "x-w17-caller-domain", "authorization"} {
		if len(md.Get(k)) > 0 {
			t.Errorf("forwarded %s", k)
		}
	}
	out, _ := metadata.FromOutgoingContext(privatesurface.ToBackend(context.Background(), alpha, md))
	if out.Get(privatesurface.CallerDomainKey)[0] != "alpha" || out.Get("x-w17-dpop-jkt")[0] != "jkt1" {
		t.Errorf("to the backend: %v", out)
	}
	ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-w17-product", "beta"))
	if _, err := privatesurface.Forwarded(ctx, alpha); status.Code(err) != codes.PermissionDenied {
		t.Errorf("alpha asking as beta: %v", err)
	}
}

// A business tier serving a person's request reaches another domain with
// what its gateway stated about that request: Dial relays the incoming
// stamps onto the outgoing call, as every inter-tier dial does.
func TestDialRelaysTheGatewaysStamps(t *testing.T) {
	look := pki(t, map[string]string{
		"proxy":  privatesurface.DefaultProxyID,
		"caller": "spiffe://w17/engine/engine-business",
	})
	srvTLS, err := privatesurface.ServerTLS(look["proxy"])
	if err != nil {
		t.Fatal(err)
	}
	var seen metadata.MD
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(srvTLS)), grpc.UnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			seen, _ = metadata.FromIncomingContext(ctx)
			return h(ctx, req)
		}))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	_, port, _ := net.SplitHostPort(lis.Addr().String())
	conn, err := privatesurface.Dial("localhost:"+port, look["caller"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// The handler's incoming call, as its gateway stamped it.
	in := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-w17-dpop-jkt", "jkt-of-the-device"))
	ctx, cancel := context.WithTimeout(in, 5*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("x-w17-dpop-jkt"); len(got) != 1 || got[0] != "jkt-of-the-device" {
		t.Errorf("the private surface saw x-w17-dpop-jkt %v, want the gateway's stamp relayed", got)
	}
}
