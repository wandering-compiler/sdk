package observx

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// The "client pair" half of the mesh posture: a collector that requires
// mutual TLS receives the export only when the exporter presents the
// mesh's client certificate (W17_INTERNAL_TLS_CERT / _KEY), and nothing
// without it.
func TestOTLPExport_PresentsTheMeshClientPair(t *testing.T) {
	certPath, keyPath := writeLoopbackCert(t)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	col, addr := startCollector(t, grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	})))

	t.Setenv("W17_INTERNAL_TLS", "on")
	t.Setenv("W17_INTERNAL_TLS_CA", certPath)
	t.Setenv(EnvOTelTLS, "")

	// Without the pair the handshake is refused: nothing arrives.
	t.Setenv("W17_INTERNAL_TLS_CERT", "")
	t.Setenv("W17_INTERNAL_TLS_KEY", "")
	_ = exportOneSpan(t, addr, Config{})
	if n := col.exports.Load(); n != 0 {
		t.Fatalf("an exporter without a client certificate reached a mutual-TLS collector (%d exports)", n)
	}

	t.Setenv("W17_INTERNAL_TLS_CERT", certPath)
	t.Setenv("W17_INTERNAL_TLS_KEY", keyPath)
	_ = exportOneSpan(t, addr, Config{})
	if col.exports.Load() == 0 {
		t.Fatal("no export reached the mutual-TLS collector with the mesh client pair set")
	}
}
