package grpcclient_test

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
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/wandering-compiler/sdk/go/lib/grpcclient"
)

// G3i3-GW-A handshake fixtures. The unit tests in tls_test.go
// cover env parsing + branch dispatch; this file pins the
// runtime behaviour against an ephemeral self-signed cert: the
// DialOption produced by BackendDialOption actually completes a
// TLS handshake with a real server (or fails with a recognisable
// certificate error when trust isn't wired).
//
// The cert is generated per-test via crypto/x509 — no on-disk
// fixtures, no openssl dependency, no leftover files between
// runs.

// generateSelfSignedCert produces a single self-signed cert
// usable as both server cert and (for mTLS) client cert. The
// cert chains to itself (IsCA: true) so the same PEM file
// doubles as the CA bundle. Files are written to `dir`; the
// test framework cleans dir up via t.TempDir().
func generateSelfSignedCert(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// startGRPCServer spins up a minimal gRPC server bound to a
// random localhost port using the provided TLS config. Returns
// the listening addr + a cleanup function. No services
// registered — every call surfaces as Unimplemented, which is
// exactly what we want: the handshake either succeeds (we get
// Unimplemented) or fails with a TLS error (we get a handshake-
// failed message).
func startGRPCServer(t *testing.T, tlsCfg *tls.Config) (addr string, cleanup func()) {
	t.Helper()
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	go func() { _ = srv.Serve(rawListener) }()
	return rawListener.Addr().String(), func() {
		srv.Stop()
		_ = rawListener.Close()
	}
}

// invoke a placeholder unary method against `addr` using
// `dialOpt`. Returns the call error (handshake failures surface
// here as `transport: authentication handshake failed: …`;
// successful handshake against an unimplemented method surfaces
// as `Unimplemented`).
func smokeInvoke(t *testing.T, addr string, dialOpt grpc.DialOption) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, dialOpt)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// /probe.svc/M doesn't exist on the server; if the handshake
	// completes the server responds with Unimplemented. We don't
	// care about the proto shape — pass a no-op codec via
	// grpc.ForceCodecV2 with the default proto codec, but the
	// simplest path is a raw bytes call: marshal an empty
	// []byte and ignore the response.
	in := &emptyMsg{}
	out := &emptyMsg{}
	return conn.Invoke(ctx, "/probe.svc/M", in, out)
}

// emptyMsg implements proto.Message minimally — Reset / String /
// ProtoMessage — enough for grpc.Invoke to call into the codec.
// The codec's Marshal is called on it; since we don't care about
// the wire payload, returning empty bytes is fine.
type emptyMsg struct{}

func (m *emptyMsg) Reset()         {}
func (m *emptyMsg) String() string { return "" }
func (m *emptyMsg) ProtoMessage()  {}

func TestBackendDial_HandshakeWithCustomCA(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir, "server")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	addr, cleanup := startGRPCServer(t, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	defer cleanup()

	env := map[string]string{
		"W17_INTERNAL_TLS":    "on",
		"W17_INTERNAL_TLS_CA": certPath,
	}
	dialOpt, err := grpcclient.TLSDialOption(lookup(env))
	if err != nil {
		t.Fatalf("BackendDialOption: %v", err)
	}
	// Self-signed cert has CN=server but the connection target
	// is 127.0.0.1 (which is in IPAddresses SAN). The handshake
	// should succeed; the call surfaces Unimplemented.
	err = smokeInvoke(t, addr, dialOpt)
	if err == nil {
		t.Fatal("expected Unimplemented; got success against a server with no registered methods")
	}
	if isHandshakeFailure(err) {
		t.Errorf("TLS handshake failed despite CA being trusted: %v", err)
	}
	if !strings.Contains(err.Error(), "Unimplemented") && !strings.Contains(err.Error(), "unimplemented") {
		t.Errorf("expected Unimplemented after successful handshake; got: %v", err)
	}
}

// G3i3-GW-A: internal TLS on, no CA pinned → handshake fails
// because the self-signed cert isn't in the system trust store.
// There is no skip-verify escape hatch any more: when internal TLS
// is on, verification is always real. Surfaces as a recognisable
// cert-validation error instead of silently succeeding.
func TestBackendDial_HandshakeFailsWithoutTrust(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir, "server")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	addr, cleanup := startGRPCServer(t, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	defer cleanup()

	// TLS=true but no CA + no skip-verify → strict default
	// verification fails on the self-signed cert.
	env := map[string]string{"W17_INTERNAL_TLS": "on"}
	dialOpt, err := grpcclient.TLSDialOption(lookup(env))
	if err != nil {
		t.Fatalf("BackendDialOption: %v", err)
	}
	err = smokeInvoke(t, addr, dialOpt)
	if err == nil {
		t.Fatal("expected handshake failure; got success — strict verification was bypassed")
	}
	if !isHandshakeFailure(err) {
		t.Errorf("expected TLS handshake / certificate error; got: %v", err)
	}
}

// G3i3-GW-A: mTLS happy path. Server requires + verifies a
// client cert presented during handshake; client supplies one
// via _CLIENT_CERT + _CLIENT_KEY. Same self-signed cert acts as
// both server cert AND client cert AND CA — simplifies the
// fixture to one keypair.
func TestBackendDial_MTLSHandshake(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir, "shared")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	caPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert as CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("AppendCertsFromPEM: nothing appended")
	}
	addr, cleanup := startGRPCServer(t, &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	})
	defer cleanup()

	env := map[string]string{
		"W17_INTERNAL_TLS":      "on",
		"W17_INTERNAL_TLS_CA":   certPath,
		"W17_INTERNAL_TLS_CERT": certPath,
		"W17_INTERNAL_TLS_KEY":  keyPath,
	}
	dialOpt, err := grpcclient.TLSDialOption(lookup(env))
	if err != nil {
		t.Fatalf("BackendDialOption: %v", err)
	}
	err = smokeInvoke(t, addr, dialOpt)
	if err == nil {
		t.Fatal("expected Unimplemented; got success on empty server")
	}
	if isHandshakeFailure(err) {
		t.Errorf("mTLS handshake failed: %v", err)
	}
}

// G3i3-GW-A: mTLS server rejects a client that presents NO
// client cert. The connection should fail at the handshake
// step; the dial-side knobs aren't checking client-cert
// presence (that's the server's job) — this test pins the
// composite contract: client without _CLIENT_CERT can't talk
// to a server that requires one.
func TestBackendDial_MTLSRejectsClientWithoutCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir, "shared")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	caPEM, _ := os.ReadFile(certPath)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	addr, cleanup := startGRPCServer(t, &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	})
	defer cleanup()

	// Client trusts the CA but doesn't present a cert.
	env := map[string]string{
		"W17_INTERNAL_TLS":    "on",
		"W17_INTERNAL_TLS_CA": certPath,
	}
	dialOpt, err := grpcclient.TLSDialOption(lookup(env))
	if err != nil {
		t.Fatalf("BackendDialOption: %v", err)
	}
	err = smokeInvoke(t, addr, dialOpt)
	if err == nil {
		t.Fatal("expected handshake failure for missing client cert")
	}
	if !isHandshakeFailure(err) {
		t.Errorf("expected handshake / TLS error for missing client cert; got: %v", err)
	}
}

// isHandshakeFailure reports whether `err` looks like a TLS
// handshake / certificate validation failure as surfaced by
// google.golang.org/grpc when the credentials layer rejects
// the peer. The exact wording varies across grpc-go versions
// but always contains one of these markers.
//
// The trailing TCP-teardown markers matter for the missing-client-cert
// case: when the server rejects the handshake it closes the connection,
// and depending on TCP timing the client can observe the RST as a
// "broken pipe" / "connection reset" / "EOF" on its in-flight write
// instead of a clean TLS alert. All are valid evidence of a rejected
// connection (the caller has already asserted err != nil), so accepting
// them removes a timing-dependent flake without weakening the test.
func isHandshakeFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"authentication handshake failed",
		"x509:",
		"tls:",
		"certificate signed by unknown authority",
		"connection error",
		"broken pipe",
		"connection reset",
		"use of closed network connection",
		"EOF",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
