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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/wandering-compiler/sdk/go/lib/grpcserver"
	"github.com/wandering-compiler/sdk/go/service/healthcheck"
)

// writeLeafEKU is writeLeaf with the extended key usages chosen.
func writeLeafEKU(t *testing.T, dir, dnsName string, eku ...x509.ExtKeyUsage) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           eku,
		IsCA:                  true,
		BasicConstraintsValid: true,
		DNSNames:              []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, dnsName+".crt")
	keyPath = filepath.Join(dir, dnsName+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// mtlsProbe stands up a listener that requires client certificates issued
// by the leaf itself and probes it with the container's env.
func mtlsProbe(t *testing.T, certPath, keyPath string) (string, error) {
	t.Helper()
	env := map[string]string{
		"W17_INTERNAL_TLS":           "on",
		"W17_INTERNAL_TLS_CERT":      certPath,
		"W17_INTERNAL_TLS_KEY":       keyPath,
		"W17_INTERNAL_TLS_CA":        certPath,
		"W17_INTERNAL_TLS_CLIENT_CA": certPath,
	}
	getenv := func(k string) string { return env[k] }
	creds, on, err := grpcserver.TLSServerOption(getenv)
	if err != nil || !on {
		t.Fatalf("TLSServerOption = (%v, %v)", on, err)
	}
	addr := listenGRPC(t, grpc_health_v1.HealthCheckResponse_SERVING, creds)
	var out bytes.Buffer
	err = healthcheck.Run(context.Background(), nil, healthcheck.Options{
		Probes: []healthcheck.Probe{healthcheck.GRPC("grpc", addr)},
		Out:    &out,
		Getenv: getenv,
	})
	return out.String(), err
}

// Under mutual TLS the probe presents this node's leaf as its client
// certificate and passes against a listener that verifies clients.
func TestRun_GRPCUnderMutualTLS(t *testing.T) {
	cert, key := writeLeafEKU(t, t.TempDir(), "app-storage.internal", x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
	if out, err := mtlsProbe(t, cert, key); err != nil {
		t.Fatalf("mTLS probe failed: %v\n%s", err, out)
	}
}

// A leaf issued for server use only cannot be a client certificate; the
// probe says so, naming the cause, instead of failing the handshake with an
// opaque error.
func TestRun_GRPCUnderMutualTLS_ServerOnlyLeafIsDiagnosed(t *testing.T) {
	cert, key := writeLeafEKU(t, t.TempDir(), "app-storage.internal", x509.ExtKeyUsageServerAuth)
	out, err := mtlsProbe(t, cert, key)
	if err == nil {
		t.Fatalf("probe passed with a server-only leaf under mTLS\n%s", out)
	}
	if msg := out + err.Error(); !strings.Contains(msg, "ClientAuth") || !strings.Contains(msg, "W17_INTERNAL_TLS_CLIENT_CA") {
		t.Errorf("failure does not name the missing ClientAuth usage: err=%v\n%s", err, out)
	}
}

// Under "terminated" the listener is plaintext, and so is the probe.
func TestRun_GRPCUnderTerminatedIsPlaintext(t *testing.T) {
	addr := listenGRPC(t, grpc_health_v1.HealthCheckResponse_SERVING)
	var out bytes.Buffer
	if err := healthcheck.Run(context.Background(), nil, healthcheck.Options{
		Probes: []healthcheck.Probe{healthcheck.GRPC("grpc", addr)},
		Out:    &out,
		Getenv: func(k string) string {
			if k == "W17_INTERNAL_TLS" {
				return "terminated"
			}
			return ""
		},
	}); err != nil {
		t.Fatalf("terminated probe of a plaintext listener failed: %v\n%s", err, out.String())
	}
}
