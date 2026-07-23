package grpcx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDialOpts_InsecureVsTLS(t *testing.T) {
	// Both postures return the five base opts (stats handler, principal-
	// forwarding unary + stream interceptors, service config, keepalive)
	// plus a transport-credentials opt = 6.
	insecureOpts := DialOpts(false)
	if len(insecureOpts) != 6 {
		t.Errorf("DialOpts(false) len = %d, want 6", len(insecureOpts))
	}
	tlsOpts := DialOpts(true)
	if len(tlsOpts) != 6 {
		t.Errorf("DialOpts(true) len = %d, want 6", len(tlsOpts))
	}
}

func TestClientTLSConfig_Arms(t *testing.T) {
	// default: no env knobs → TLS1.2 floor, verify on, system roots
	cfg := clientTLSConfig(func(string) string { return "" })
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS1.2", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Error("default config should verify")
	}
	if cfg.RootCAs != nil {
		t.Error("default config should use system roots (nil RootCAs)")
	}

	// There is no skip-verify knob any more: when internal TLS is on,
	// verification is always real. The config never sets InsecureSkipVerify.

	// valid CA bundle → RootCAs populated
	caPath := writeTestCA(t)
	withCA := clientTLSConfig(func(k string) string {
		if k == "W17_INTERNAL_TLS_CA" {
			return caPath
		}
		return ""
	})
	if withCA.RootCAs == nil {
		t.Error("valid CA path should populate RootCAs")
	}
	if withCA.InsecureSkipVerify {
		t.Error("CA config must still verify (no skip-verify)")
	}

	// missing CA path → falls back to system roots (no error)
	missing := clientTLSConfig(func(k string) string {
		if k == "W17_INTERNAL_TLS_CA" {
			return filepath.Join(t.TempDir(), "nope.pem")
		}
		return ""
	})
	if missing.RootCAs != nil {
		t.Error("missing CA path should fall back to system roots")
	}

	// CA file with no valid PEM → AppendCertsFromPEM fails → system roots
	badPath := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPath, []byte("not a pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := clientTLSConfig(func(k string) string {
		if k == "W17_INTERNAL_TLS_CA" {
			return badPath
		}
		return ""
	})
	if bad.RootCAs != nil {
		t.Error("non-PEM CA file should leave RootCAs nil")
	}
}

// writeTestCA generates a self-signed cert and writes it as a PEM bundle,
// returning the path.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31-1, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	return path
}
