package grpcserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// A listener built FROM TLSServerOption with a client CA MUST refuse, in a
// real handshake, a client that presents no certificate and one whose
// certificate another CA issued, and serve one holding a leaf of the client
// CA. The option is opaque (a grpc.ServerOption), so only a handshake against
// it proves which ClientAuth it carries: weakening RequireAndVerifyClientCert
// to an optional mode must turn this red.
func TestTLSServerOption_MutualTLSRefusesAClientWithoutAValidCertificate(t *testing.T) {
	cert, key, ca := certDir(t)
	env := map[string]string{
		"W17_INTERNAL_TLS":           "on",
		"W17_INTERNAL_TLS_CERT":      cert,
		"W17_INTERNAL_TLS_KEY":       key,
		"W17_INTERNAL_TLS_CLIENT_CA": ca,
	}
	opt, enabled, err := TLSServerOption(func(k string) string { return env[k] })
	if err != nil || !enabled {
		t.Fatalf("mTLS config: enabled=%v err=%v", enabled, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(opt)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	caPEM, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	leaf := func(certPath, keyPath string) []tls.Certificate {
		c, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		return []tls.Certificate{c}
	}
	otherCert, otherKey, _ := certDir(t) // a second, unrelated dev CA

	check := func(certs []tls.Certificate) error {
		creds := credentials.NewTLS(&tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      roots,
			ServerName:   "localhost",
			Certificates: certs,
		})
		conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(creds))
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		return err
	}

	if err := check(nil); err == nil {
		t.Fatal("a client presenting no certificate was served by a listener with a client CA")
	}
	if err := check(leaf(otherCert, otherKey)); err == nil {
		t.Fatal("a client whose certificate another CA issued was served")
	}
	if err := check(leaf(cert, key)); err != nil {
		t.Fatalf("a client holding a leaf of the client CA was refused: %v", err)
	}
}
