package certgen

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCertsWritesFullPKI(t *testing.T) {
	dir := t.TempDir()
	written, err := EnsureCerts(dir, []string{"localhost", "app-storage", "10.0.0.5"})
	if err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}
	if len(written) != 4 {
		t.Fatalf("fresh dir: want 4 files written, got %d (%v)", len(written), written)
	}
	for _, f := range []string{caFile, caKeyFile, serverCertFile, serverKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s on disk: %v", f, err)
		}
	}
}

func TestEnsureCertsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatalf("first EnsureCerts: %v", err)
	}
	written, err := EnsureCerts(dir, []string{"localhost"})
	if err != nil {
		t.Fatalf("second EnsureCerts: %v", err)
	}
	if len(written) != 0 {
		t.Fatalf("second run should write nothing, wrote %v", written)
	}
}

func TestEnsureCertsReusesCAForMissingLeaf(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	caBefore, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	// Drop the leaf; the CA must be reused (not regenerated) to sign a new one.
	if err := os.Remove(filepath.Join(dir, serverCertFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, serverKeyFile)); err != nil {
		t.Fatal(err)
	}
	written, err := EnsureCerts(dir, []string{"localhost"})
	if err != nil {
		t.Fatalf("refill leaf: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("want 2 leaf files rewritten, got %v", written)
	}
	caAfter, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(caBefore) != string(caAfter) {
		t.Fatal("CA was regenerated; it must be reused so distributed trust stays valid")
	}
}

func TestLeafVerifiesAgainstCAWithSANs(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"app-storage"}); err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}
	caCert := parsePEMCert(t, filepath.Join(dir, caFile))
	leaf := parsePEMCert(t, filepath.Join(dir, serverCertFile))

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "app-storage"}); err != nil {
		t.Fatalf("leaf failed to verify against CA for SAN app-storage: %v", err)
	}
	// localhost is always present so loopback dials work.
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Fatalf("leaf must carry localhost SAN: %v", err)
	}
}

func parsePEMCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("%s: no PEM block", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("%s: parse: %v", path, err)
	}
	return cert
}
