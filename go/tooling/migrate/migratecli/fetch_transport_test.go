package migratecli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// fakeFetch is a console's fetch surface that records the bearer of every
// call it receives and answers with err (nil = an empty set).
type fakeFetch struct {
	applyfetchpb.UnimplementedMigrationFetchServer
	err error

	mu      sync.Mutex
	bearers []string
}

func (f *fakeFetch) FetchMigrations(ctx context.Context, _ *applyfetchpb.FetchMigrationsRequest) (*applyfetchpb.FetchMigrationsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.bearers = append(f.bearers, strings.Join(md.Get("authorization"), ","))
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &applyfetchpb.FetchMigrationsResponse{}, nil
}

func (f *fakeFetch) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bearers...)
}

// serveFetch runs f on a loopback port, over TLS when cert is non-nil.
func serveFetch(t *testing.T, f *fakeFetch, cert *tls.Certificate) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var opts []grpc.ServerOption
	if cert != nil {
		opts = append(opts, grpc.Creds(credentials.NewServerTLSFromCert(cert)))
	}
	srv := grpc.NewServer(opts...)
	applyfetchpb.RegisterMigrationFetchServer(srv, f)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

// selfSigned mints a certificate for 127.0.0.1 and writes its PEM to a file.
func selfSigned(t *testing.T) (*tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "private console"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, path
}

func fetchEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

var pinned = []migrate.ConnTarget{{Connection: "main", TargetMigrationID: "0001"}}

func fetchWithin(t *testing.T, env map[string]string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := fetchMigrations(ctx, "p-1", pinned, fetchEnv(env))
	return err
}

// The default transport is TLS verified against the system roots: a bare
// host:port and grpcs:// both refuse a console whose certificate no system
// root vouches for, and W17_CONSOLE_CA is what makes a private one trusted.
func TestFetch_DefaultTransportIsTLSWithSystemRoots(t *testing.T) {
	cert, caPath := selfSigned(t)
	f := &fakeFetch{}
	addr := serveFetch(t, f, cert)

	for _, a := range []string{addr, "grpcs://" + addr} {
		if err := fetchWithin(t, map[string]string{envConsoleAddr: a, envToken: "tok"}); err == nil {
			t.Errorf("%s: a certificate no system root vouches for was accepted", a)
		}
	}
	if got := f.calls(); len(got) != 0 {
		t.Fatalf("the console was reached over an unverified TLS link: %v", got)
	}
	if err := fetchWithin(t, map[string]string{envConsoleAddr: addr, envToken: "tok", envConsoleCA: caPath}); err != nil {
		t.Fatalf("with W17_CONSOLE_CA naming the private CA: %v", err)
	}
	if got := f.calls(); len(got) != 1 || got[0] != "Bearer tok" {
		t.Fatalf("calls over the verified link = %v, want one with the bearer", got)
	}
}

// The bearer is never sent in clear by default: a bare host:port against a
// plaintext console fails before any call reaches it, and only an explicit
// grpc:// sends the token without TLS.
func TestFetch_BearerIsNotSentInClearByDefault(t *testing.T) {
	f := &fakeFetch{}
	addr := serveFetch(t, f, nil)

	if err := fetchWithin(t, map[string]string{envConsoleAddr: addr, envToken: "secret"}); err == nil {
		t.Fatal("a bare host:port fetched from a plaintext console")
	}
	if got := f.calls(); len(got) != 0 {
		t.Fatalf("the bearer reached a plaintext console without an explicit grpc://: %v", got)
	}
	if err := fetchWithin(t, map[string]string{envConsoleAddr: "grpc://" + addr, envToken: "secret"}); err != nil {
		t.Fatalf("explicit grpc://: %v", err)
	}
	if got := f.calls(); len(got) != 1 || got[0] != "Bearer secret" {
		t.Fatalf("calls over explicit grpc:// = %v, want one with the bearer", got)
	}
}

// An Unauthenticated reply names the variable to set.
func TestFetch_UnauthenticatedNamesW17Token(t *testing.T) {
	f := &fakeFetch{err: status.Error(codes.Unauthenticated, "no token")}
	addr := serveFetch(t, f, nil)
	err := fetchWithin(t, map[string]string{envConsoleAddr: "grpc://" + addr})
	if err == nil {
		t.Fatal("an Unauthenticated reply was not an error")
	}
	if !strings.Contains(err.Error(), "W17_TOKEN") {
		t.Fatalf("the refusal does not name W17_TOKEN:\n%v", err)
	}
}
