package certgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"strings"
	"testing"
	"time"
)

// mintSelfSigned creates a self-signed cert from tmpl + a fresh key and parses
// it back, so validateReusableCA can be driven with precise CA flags / validity.
func mintSelfSigned(t *testing.T, tmpl *x509.Certificate) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl.SerialNumber = big.NewInt(1)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert, key
}

// TestValidateReusableCA covers every arm: a valid CA accepted, and the three
// fail-closed refusals (not a signing CA, expired, private key mismatch) that
// force a re-mint rather than silently reusing a broken CA.
func TestValidateReusableCA(t *testing.T) {
	now := time.Now()

	// Valid signing CA → accepted.
	goodCert, goodKey := mintSelfSigned(t, &x509.Certificate{
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
	})
	if err := validateReusableCA(goodCert, goodKey, "ca.crt", "ca.key"); err != nil {
		t.Errorf("valid CA rejected: %v", err)
	}

	// Not a signing CA (IsCA false) → refused.
	leafCert, leafKey := mintSelfSigned(t, &x509.Certificate{
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
	})
	if err := validateReusableCA(leafCert, leafKey, "ca.crt", "ca.key"); err == nil ||
		!strings.Contains(err.Error(), "signing CA") {
		t.Errorf("non-CA: want signing-CA refusal, got %v", err)
	}

	// Expired CA → refused.
	expCert, expKey := mintSelfSigned(t, &x509.Certificate{
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		NotBefore: now.Add(-48 * time.Hour), NotAfter: now.Add(-time.Hour),
	})
	if err := validateReusableCA(expCert, expKey, "ca.crt", "ca.key"); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: want expiry refusal, got %v", err)
	}

	// Key mismatch: a valid CA cert but the wrong private key → refused.
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	if err := validateReusableCA(goodCert, otherKey, "ca.crt", "ca.key"); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Errorf("mismatch: want key-mismatch refusal, got %v", err)
	}
}
