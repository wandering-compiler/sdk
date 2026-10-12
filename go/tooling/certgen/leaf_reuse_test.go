package certgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func readLeaf(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	cert, _, err := loadCert(filepath.Join(dir, serverCertFile), filepath.Join(dir, serverKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func readCA(t *testing.T, dir string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	cert, key, err := loadCert(filepath.Join(dir, caFile), filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// After the CA pair is deleted and re-minted, the old leaf no longer
// chains to it and must be re-signed.
func TestEnsureLeaf_ReMintedAfterCAReMint(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"api"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{caFile, caKeyFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	written, err := EnsureCerts(dir, []string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(written, filepath.Join(dir, serverCertFile)) {
		t.Errorf("leaf not re-minted after a CA re-mint; wrote %v", written)
	}
	ca, _ := readCA(t, dir)
	if err := readLeaf(t, dir).CheckSignatureFrom(ca); err != nil {
		t.Errorf("leaf does not chain to the current CA: %v", err)
	}
}

// A leaf missing a newly requested SAN is re-minted to cover it.
func TestEnsureLeaf_ReMintedForANewSAN(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"api"}); err != nil {
		t.Fatal(err)
	}
	written, err := EnsureCerts(dir, []string{"api", "worker", "10.0.0.7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Errorf("want the leaf pair re-written, got %v", written)
	}
	leaf := readLeaf(t, dir)
	if !slices.Contains(leaf.DNSNames, "worker") || !slices.ContainsFunc(leaf.IPAddresses, net.ParseIP("10.0.0.7").Equal) {
		t.Errorf("new SANs missing: dns=%v ip=%v", leaf.DNSNames, leaf.IPAddresses)
	}
	// A subset of what the leaf covers changes nothing.
	if written, err := EnsureCerts(dir, []string{"worker"}); err != nil || len(written) != 0 {
		t.Errorf("a covered SAN set must keep the leaf: wrote %v, err %v", written, err)
	}
}

// An expired leaf is re-minted.
func TestEnsureLeaf_ReMintedWhenExpired(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, nil); err != nil {
		t.Fatal(err)
	}
	ca, caKey := readCA(t, dir)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "old"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCertPEM(filepath.Join(dir, serverCertFile), der); err != nil {
		t.Fatal(err)
	}
	if err := writeKeyPEM(filepath.Join(dir, serverKeyFile), key); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCerts(dir, nil); err != nil {
		t.Fatal(err)
	}
	if leaf := readLeaf(t, dir); !time.Now().Before(leaf.NotAfter) {
		t.Errorf("expired leaf kept (NotAfter %s)", leaf.NotAfter)
	}
}

// An unreadable leaf is refused, not overwritten.
func TestEnsureLeaf_UnreadableIsRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, serverCertFile)
	if err := os.WriteFile(p, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCerts(dir, nil); err == nil {
		t.Fatal("an unreadable leaf must be refused")
	}
	if b, _ := os.ReadFile(p); string(b) != "garbage" {
		t.Error("an unreadable leaf was overwritten")
	}
}

// A leaf issued for server use only is re-minted: the node also presents
// it as its client certificate under mutual TLS, and a listener that
// verifies clients refuses a leaf without ClientAuth.
func TestEnsureLeaf_ReMintedWithoutClientAuth(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, nil); err != nil {
		t.Fatal(err)
	}
	ca, caKey := readCA(t, dir)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(8),
		Subject:      pkix.Name{CommonName: "server-only"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCertPEM(filepath.Join(dir, serverCertFile), der); err != nil {
		t.Fatal(err)
	}
	if err := writeKeyPEM(filepath.Join(dir, serverKeyFile), key); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCerts(dir, nil); err != nil {
		t.Fatal(err)
	}
	if leaf := readLeaf(t, dir); !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Errorf("server-only leaf kept: EKU %v", leaf.ExtKeyUsage)
	}
}
