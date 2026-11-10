package privatesurface_test

// Every refusal branch of the private surface: who the gateway serves (the
// proxy's certificate, exactly one well-formed forwarded caller), which
// identity it accepts as the proxy, what SelfTLS takes for "this bundle's
// own listener", and the material a hop needs.

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/privatesurface"
	"github.com/wandering-compiler/sdk/go/tooling/certgen"
)

// servePrivate serves a health service behind ServerTLS(look), running check
// on every call before the handler; it returns the address.
func servePrivate(t *testing.T, look privatesurface.LookupFunc, check func(context.Context) error) string {
	t.Helper()
	srvTLS, err := privatesurface.ServerTLS(look)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(srvTLS)), grpc.UnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if check != nil {
				if err := check(ctx); err != nil {
					return nil, err
				}
			}
			return h(ctx, req)
		}))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// check calls the health service at addr over cfg with md.
func check(t *testing.T, addr string, cfg *tls.Config, md metadata.MD) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), md), 3*time.Second)
	defer cancel()
	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	return err
}

// The gateway serves a call only from the proxy's certificate carrying
// exactly one well-formed caller; the proxy identity it accepts is
// W17_PRIVATE_PROXY_ID when set. Everything else is Unauthenticated.
func TestCaller_RefusesAllButOneWellFormedCallerFromTheProxy(t *testing.T) {
	look := pki(t, map[string]string{
		"gateway": "spiffe://w17/identity/identity-gateway-private",
		"proxy":   privatesurface.DefaultProxyID,
	})
	proxyID := privatesurface.DefaultProxyID
	addr := servePrivate(t, look["gateway"], func(ctx context.Context) error {
		_, err := privatesurface.Caller(ctx, proxyID)
		return err
	})
	cfg, err := privatesurface.ClientTLS(look["proxy"], "identity-private")
	if err != nil {
		t.Fatal(err)
	}
	const alpha = "spiffe://w17/alpha/alpha-gateway"
	if err := check(t, addr, cfg, metadata.Pairs(privatesurface.CallerMetadataKey, alpha)); err != nil {
		t.Fatalf("one caller through the proxy: %v", err)
	}
	refused := map[string]metadata.MD{
		"two callers": metadata.Pairs(privatesurface.CallerMetadataKey, alpha, privatesurface.CallerMetadataKey, "spiffe://w17/beta/beta-gateway"),
	}
	for _, bad := range []string{"spiffe://w17/alpha", "https://w17/alpha/alpha-gateway", "spiffe://other/alpha/alpha-gateway", "alpha", ""} {
		refused["malformed "+bad] = metadata.Pairs(privatesurface.CallerMetadataKey, bad)
	}
	for name, md := range refused {
		if err := check(t, addr, cfg, md); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: %v, want Unauthenticated", name, err)
		}
	}
	// A gateway told to accept another proxy refuses the default one.
	proxyID = privatesurface.ProxyID(func(k string) string {
		if k == privatesurface.EnvProxyID {
			return " spiffe://w17/proxy/other "
		}
		return ""
	})
	if proxyID != "spiffe://w17/proxy/other" {
		t.Fatalf("ProxyID with the override = %q", proxyID)
	}
	if err := check(t, addr, cfg, metadata.Pairs(privatesurface.CallerMetadataKey, alpha)); status.Code(err) != codes.Unauthenticated {
		t.Errorf("the default proxy against an overridden proxy id: %v, want Unauthenticated", err)
	}
	if got := privatesurface.ProxyID(func(string) string { return "" }); got != privatesurface.DefaultProxyID {
		t.Errorf("ProxyID unset = %q, want the default", got)
	}
}

// plainAuth is a peer that connected without TLS.
type plainAuth struct{}

func (plainAuth) AuthType() string { return "insecure" }

// Without a verified TLS peer there is no proxy to trust.
func TestCaller_RefusesACallWithoutAVerifiedTLSPeer(t *testing.T) {
	md := metadata.Pairs(privatesurface.CallerMetadataKey, "spiffe://w17/alpha/alpha-gateway")
	for name, ctx := range map[string]context.Context{
		"no peer":     metadata.NewIncomingContext(context.Background(), md),
		"plaintext":   peer.NewContext(metadata.NewIncomingContext(context.Background(), md), &peer.Peer{AuthInfo: plainAuth{}}),
		"unverified":  peer.NewContext(metadata.NewIncomingContext(context.Background(), md), &peer.Peer{AuthInfo: credentials.TLSInfo{}}),
		"nil authinf": peer.NewContext(metadata.NewIncomingContext(context.Background(), md), &peer.Peer{}),
	} {
		if _, err := privatesurface.Caller(ctx, privatesurface.DefaultProxyID); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: %v, want Unauthenticated", name, err)
		}
	}
}

// The gateway's listener requires a client certificate of the private CA:
// none, or one from another CA, ends the handshake.
func TestServerTLS_RefusesAClientOutsideThePrivateCA(t *testing.T) {
	look := pki(t, map[string]string{"gateway": "spiffe://w17/identity/identity-gateway-private"})
	addr := servePrivate(t, look["gateway"], nil)
	foreign := pki(t, map[string]string{"proxy": privatesurface.DefaultProxyID})
	cfg, err := privatesurface.ClientTLS(foreign["proxy"], "identity-private")
	if err != nil {
		t.Fatal(err)
	}
	// The foreign client trusts the gateway's CA, so only the server's
	// check of the client certificate can refuse it.
	ca, err := os.ReadFile(look["gateway"](privatesurface.EnvCA))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RootCAs.AppendCertsFromPEM(ca) {
		t.Fatal("gateway CA")
	}
	if err := check(t, addr, cfg, nil); err == nil {
		t.Error("a client certificate from another CA was served")
	}
	noCert := tlsConfigInsecure()
	if err := check(t, addr, &noCert, nil); err == nil {
		t.Error("a client without a certificate was served")
	}
}

// SelfTLS accepts only this bundle's own listener: a chain of the private CA
// AND a leaf carrying this bundle's URI. A foreign CA's leaf with the same
// URI, or the private CA's leaf of another bundle, is refused; a leaf with no
// URI SAN cannot be used for it at all.
func TestSelfTLS_AcceptsOnlyThisBundlesListener(t *testing.T) {
	const uri = "spiffe://w17/engine/engine-gateway-private"
	dir := t.TempDir()
	mint := func(dir, name, uri string) privatesurface.LookupFunc {
		if _, err := certgen.EnsureIdentity(dir, name, uri, []string{"localhost"}); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{
			privatesurface.EnvCert: filepath.Join(dir, name+".crt"),
			privatesurface.EnvKey:  filepath.Join(dir, name+".key"),
			privatesurface.EnvCA:   filepath.Join(dir, "ca.crt"),
		}
		return func(k string) string { return env[k] }
	}
	own := mint(dir, "self", uri)
	sibling := mint(dir, "sibling", "spiffe://w17/engine/engine-business")
	impostor := mint(t.TempDir(), "impostor", uri)

	self, err := privatesurface.SelfTLS(own)
	if err != nil {
		t.Fatal(err)
	}
	// A server-side TLS listener that does not ask for a client
	// certificate, so only SelfTLS's check of the listener decides.
	serve := func(look privatesurface.LookupFunc) string {
		cfg, err := privatesurface.ServerTLS(look)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ClientAuth = tls.NoClientCert
		srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
		healthpb.RegisterHealthServer(srv, health.NewServer())
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		return lis.Addr().String()
	}
	if err := check(t, serve(own), self, nil); err != nil {
		t.Fatalf("the bundle's own listener: %v", err)
	}
	if err := check(t, serve(impostor), self, nil); err == nil {
		t.Error("a listener of a foreign CA carrying this bundle's URI was accepted")
	}
	if err := check(t, serve(sibling), self, nil); err == nil {
		t.Error("a listener of another bundle was accepted")
	}

	// A leaf without a URI SAN is not an identity.
	if _, err := certgen.EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	noURI := map[string]string{
		privatesurface.EnvCert: filepath.Join(dir, "server.crt"),
		privatesurface.EnvKey:  filepath.Join(dir, "server.key"),
		privatesurface.EnvCA:   filepath.Join(dir, "ca.crt"),
	}
	if _, err := privatesurface.SelfTLS(func(k string) string { return noURI[k] }); err == nil {
		t.Error("SelfTLS took a leaf with no URI SAN")
	}
	if _, err := privatesurface.SelfTLS(func(string) string { return "" }); err == nil {
		t.Error("SelfTLS without material")
	}
}

// Every hop needs the whole material, readable: a missing variable, an
// unreadable CA or one holding no certificate is an error before anything
// listens or dials.
func TestMaterial_RefusesWhatIsMissingOrUnreadable(t *testing.T) {
	look := pki(t, map[string]string{"x": "spiffe://w17/alpha/alpha-gateway"})["x"]
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	with := func(k, v string) privatesurface.LookupFunc {
		return func(key string) string {
			if key == k {
				return v
			}
			return look(key)
		}
	}
	for name, l := range map[string]privatesurface.LookupFunc{
		"no cert":        with(privatesurface.EnvCert, ""),
		"no key":         with(privatesurface.EnvKey, ""),
		"no CA":          with(privatesurface.EnvCA, ""),
		"unreadable CA":  with(privatesurface.EnvCA, filepath.Join(t.TempDir(), "missing.pem")),
		"CA without PEM": with(privatesurface.EnvCA, empty),
		"unreadable key": with(privatesurface.EnvKey, empty),
	} {
		if _, err := privatesurface.ServerTLS(l); err == nil {
			t.Errorf("ServerTLS with %s", name)
		}
		if _, err := privatesurface.ClientTLS(l, "x"); err == nil {
			t.Errorf("ClientTLS with %s", name)
		}
		if _, err := privatesurface.Dial("alpha-private:50061", l); err == nil {
			t.Errorf("Dial with %s", name)
		}
	}
	if _, err := privatesurface.Dial("no-port", look); err == nil {
		t.Error("Dial took an address without a port")
	}
}

// Dial verifies the private surface's certificate against the private CA
// under the address's host name: a server of another CA is refused.
func TestDial_RefusesAServerOutsideThePrivateCA(t *testing.T) {
	foreign := pki(t, map[string]string{"proxy": privatesurface.DefaultProxyID})
	look := pki(t, map[string]string{"caller": "spiffe://w17/engine/engine-business"})
	cfg, err := privatesurface.ServerTLS(foreign["proxy"])
	if err != nil {
		t.Fatal(err)
	}
	cfg.ClientAuth = tls.NoClientCert
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Error("Dial reached a server of another CA")
	}
}

// A product asks only for itself: one product value naming another domain
// among several is refused, and keys outside ForwardedKeys never pass.
func TestForwarded_RefusesAnyForeignProductValue(t *testing.T) {
	alpha := privatesurface.Identity{Domain: "alpha", Bundle: "alpha-admin"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-w17-product", "alpha", "x-w17-product", "beta"))
	if _, err := privatesurface.Forwarded(ctx, alpha); status.Code(err) != codes.PermissionDenied {
		t.Errorf("alpha and beta as products: %v, want PermissionDenied", err)
	}
	ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-w17-unlisted", "v"))
	md, err := privatesurface.Forwarded(ctx, alpha)
	if err != nil || len(md) != 0 {
		t.Errorf("an unlisted key: %v %v, want nothing forwarded", md, err)
	}
}

// A private hop is mutual TLS whatever the internal-mesh switch says: with
// W17_INTERNAL_TLS off, the gateway listener still requires and verifies a
// client certificate, and a caller still presents its leaf.
func TestMTLSWhateverTheMeshSwitchSays(t *testing.T) {
	look := pki(t, map[string]string{"x": "spiffe://w17/alpha/alpha-gateway"})["x"]
	off := func(k string) string {
		if k == "W17_INTERNAL_TLS" {
			return "off"
		}
		return look(k)
	}
	srv, err := privatesurface.ServerTLS(off)
	if err != nil {
		t.Fatal(err)
	}
	if srv.ClientAuth != tls.RequireAndVerifyClientCert || srv.ClientCAs == nil {
		t.Errorf("listener client auth = %v", srv.ClientAuth)
	}
	cli, err := privatesurface.ClientTLS(off, "alpha-private")
	if err != nil {
		t.Fatal(err)
	}
	if len(cli.Certificates) != 1 || cli.RootCAs == nil || cli.InsecureSkipVerify {
		t.Errorf("caller TLS = %+v", cli)
	}
}
