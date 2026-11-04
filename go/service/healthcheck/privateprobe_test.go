package healthcheck_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/wandering-compiler/sdk/go/lib/privatesurface"
	"github.com/wandering-compiler/sdk/go/service/healthcheck"
)

// privatePKI is a CA and leaves it issues, each carrying ONLY a URI SAN — the
// shape of a production private identity, with no DNS SAN and no localhost.
type privatePKI struct {
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPath string
}

func newPrivatePKI(t *testing.T) *privatePKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "private CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	dir := t.TempDir()
	p := &privatePKI{dir: dir, ca: ca, caKey: key, caPath: filepath.Join(dir, "ca.crt")}
	if err := os.WriteFile(p.caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *privatePKI) leaf(t *testing.T, name, uri string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(uri)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPath, keyPath = filepath.Join(p.dir, name+".crt"), filepath.Join(p.dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// A private listener whose identity is a SPIFFE URI with no DNS SAN — what a
// production PKI issues — is healthy to its own container. The probe used to
// verify it as "localhost", which only the dev PKI's leaves carry, so every
// production binary serving a private surface failed its health check
// (2026-11-04: a private-only domain's composed server rolled the deploy back).
// A listener holding ANOTHER identity of the same CA is not this container.
func TestRun_PrivateGRPCAcceptsItsOwnURIIdentity(t *testing.T) {
	pki := newPrivatePKI(t)
	own := "spiffe://w17/geo/geo-server"
	ownCert, ownKey := pki.leaf(t, "geo-server", own)
	env := func(cert, key string) func(string) string {
		m := map[string]string{privatesurface.EnvCert: cert, privatesurface.EnvKey: key, privatesurface.EnvCA: pki.caPath}
		return func(k string) string { return m[k] }
	}
	serve := func(lookup func(string) string) string {
		cfg, err := privatesurface.ServerTLS(privatesurface.LookupFunc(lookup))
		if err != nil {
			t.Fatal(err)
		}
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
		hs := health.NewServer()
		hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		grpc_health_v1.RegisterHealthServer(srv, hs)
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		return lis.Addr().String()
	}
	probe := func(addr string) (string, error) {
		var out bytes.Buffer
		_, err := healthcheck.Dispatch(context.Background(), []string{"health"}, healthcheck.Options{
			Probes: []healthcheck.Probe{healthcheck.PrivateGRPC("private", addr)},
			Out:    &out,
			Getenv: env(ownCert, ownKey),
		})
		return out.String(), err
	}

	if out, err := probe(serve(env(ownCert, ownKey))); err != nil {
		t.Fatalf("its own URI-only identity is not healthy: %v\n%s", err, out)
	}

	otherCert, otherKey := pki.leaf(t, "other", "spiffe://w17/geo/other")
	if out, err := probe(serve(env(otherCert, otherKey))); err == nil || !strings.Contains(out+err.Error(), "not this bundle") {
		t.Fatalf("a listener holding another identity of the same CA passed: err=%v\n%s", err, out)
	}
}
