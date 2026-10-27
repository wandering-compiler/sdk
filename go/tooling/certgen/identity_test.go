package certgen

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// A service identity is its own leaf, signed by the dev CA, carrying the
// identity URI and both key usages; a second run keeps it.
func TestEnsureIdentity(t *testing.T) {
	dir := t.TempDir()
	written, err := EnsureIdentity(dir, "alpha-gateway", "spiffe://w17/alpha/alpha-gateway", []string{"alpha-gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 4 {
		t.Fatalf("wrote %v, want the CA pair and the leaf pair", written)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "alpha-gateway.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://w17/alpha/alpha-gateway" {
		t.Errorf("URIs = %v", cert.URIs)
	}
	if len(cert.ExtKeyUsage) != 2 {
		t.Errorf("key usages = %v, want server and client", cert.ExtKeyUsage)
	}
	again, err := EnsureIdentity(dir, "alpha-gateway", "spiffe://w17/alpha/alpha-gateway", []string{"alpha-gateway"})
	if err != nil || len(again) != 0 {
		t.Errorf("second run wrote %v (%v), want nothing", again, err)
	}
	// Another identity in the same directory shares the CA.
	if w, err := EnsureIdentity(dir, "proxy", "spiffe://w17/proxy/private", nil); err != nil || len(w) != 2 {
		t.Errorf("second identity wrote %v (%v), want its leaf pair only", w, err)
	}
	for _, bad := range []string{"not a uri", ""} {
		if _, err := EnsureIdentity(dir, "x", bad, nil); err == nil {
			t.Errorf("identity %q accepted", bad)
		}
	}
	if _, err := EnsureIdentity(dir, "ca", "spiffe://w17/a/b", nil); err == nil {
		t.Error("a leaf named ca would overwrite the CA")
	}
}
