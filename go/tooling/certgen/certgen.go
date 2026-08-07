// Package certgen mints the local development PKI the
// wandering-compiler wires into every generated bundle's TLS
// posture (see [grpcserver] / [grpcclient]). It exists so a fresh
// `w17ctl init` yields a working encrypted-by-default stack with
// zero operator effort: one self-signed dev CA plus one server
// leaf the CA signs, written under `w17/certs/dev.local/`.
//
// **Scope is dev convenience, not production PKI.** The certs are
// deliberately unsafe-for-prod: a long-lived self-signed CA whose
// private key sits next to the cert on disk. Production cert
// material is the operator's / platform's job — devops points the
// `<PREFIX>_TLS_CERT` / `_KEY` / `_CA` env at certs a real CA
// (cert-manager, a private PKI, a mesh's SPIFFE identities) issues
// and rotates. [EnsureCerts] can fill a prod directory's MISSING
// files idempotently, but it never owns renewal — short-lived,
// rotated prod certs are outside this package.
//
// **Idempotent by construction.** [EnsureCerts] only writes files
// that don't already exist, so re-running `init` (or `w17ctl
// certs`) never clobbers a CA an operator has already distributed
// trust for, nor a leaf a sidecar is already serving.
package certgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// File names the package reads and writes inside a cert
// directory. Kept as consts so the CLI command, the env-defaults
// generator, and this package agree on one spelling.
const (
	caFile         = "ca.crt"
	caKeyFile      = "ca.key"
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"
	gitignoreFile  = ".gitignore"
	// lockFile is the flock(2) sentinel that serialises concurrent
	// EnsureCerts runs on one directory (Q56-tls-1). It is covered by
	// gitignoreBody's `*` ignore, so it never lands in a commit.
	lockFile = ".certgen.lock"
)

// gitignoreBody ignores everything in a cert directory (private
// keys must never be committed) while keeping the .gitignore
// itself tracked so the directory's intent stays visible in the
// tree. Dropped into every cert dir EnsureCerts touches — dev and
// prod alike.
const gitignoreBody = "# TLS material — never commit private keys.\n*\n!.gitignore\n"

// devCertValidity is the leaf + CA lifetime for dev material —
// long (10y) on purpose: a dev cert that silently expires mid-
// project is pure friction, and the key never leaves the
// developer's machine. Prod certs (short, rotated) are not this
// package's concern.
const devCertValidity = 10 * 365 * 24 * time.Hour

// EnsureCerts makes sure dir holds a usable dev PKI — a CA
// (ca.crt/ca.key) and a server leaf (server.crt/server.key) the
// CA signed, carrying sans as its SubjectAltNames. It writes only
// the files that are missing: an existing CA is reused to sign a
// missing leaf, and a complete set is left untouched. dir is
// created (0700) when absent. Returns the list of files it
// actually wrote so the caller can report what changed.
//
// sans should include every name a dialer may use to reach the
// server — "localhost", "127.0.0.1", and each compose/k8s service
// name. Empty sans falls back to localhost + 127.0.0.1.
func EnsureCerts(dir string, sans []string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("certgen: mkdir %q: %w", dir, err)
	}

	// Serialise concurrent runs on this directory so two invocations
	// can't interleave their check-then-write mint sequences into a
	// mismatched cert/key pair (Q56-tls-1). Held for the whole ensure
	// sequence; released on return.
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	gitignorePath := filepath.Join(dir, gitignoreFile)
	if !fileExists(gitignorePath) {
		if err := os.WriteFile(gitignorePath, []byte(gitignoreBody), 0o644); err != nil {
			return nil, fmt.Errorf("certgen: write %q: %w", gitignorePath, err)
		}
	}

	caCertPath := filepath.Join(dir, caFile)
	caKeyPath := filepath.Join(dir, caKeyFile)

	caCert, caKey, written, err := ensureCA(caCertPath, caKeyPath)
	if err != nil {
		return nil, err
	}

	leafWritten, err := ensureLeaf(dir, caCert, caKey, sans)
	if err != nil {
		return nil, err
	}
	return append(written, leafWritten...), nil
}

// ensureCA loads the CA from disk when both files exist, else
// mints a fresh self-signed CA and writes it. Returns the parsed
// cert + key for leaf signing plus the paths it wrote.
func ensureCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, []string, error) {
	certThere, keyThere := fileExists(certPath), fileExists(keyPath)
	if certThere && keyThere {
		cert, key, err := loadCert(certPath, keyPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("certgen: reuse CA: %w", err)
		}
		if err := validateReusableCA(cert, key, certPath, keyPath); err != nil {
			return nil, nil, nil, err
		}
		return cert, key, nil, nil
	}
	// No-clobber invariant: only mint a fresh CA when NEITHER half
	// is present. If exactly one of ca.crt / ca.key exists, minting
	// would overwrite the surviving half (and silently invalidate
	// any leaf the existing key signed / any trust the existing cert
	// was distributed for). Refuse instead — the operator must
	// restore the missing half or remove both deliberately.
	if certThere != keyThere {
		present, missing := certPath, keyPath
		if keyThere {
			present, missing = keyPath, certPath
		}
		return nil, nil, nil, fmt.Errorf(
			"certgen: CA half-present: %q exists but its pair %q is missing — refusing to overwrite (restore the missing file or remove both to re-mint)",
			present, missing)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("certgen: CA key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"wandering-compiler dev"},
			CommonName:   "w17 dev.local CA",
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(devCertValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("certgen: create CA: %w", err)
	}
	if err := writeCertPEM(certPath, der); err != nil {
		return nil, nil, nil, err
	}
	if err := writeKeyPEM(keyPath, key); err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("certgen: parse fresh CA: %w", err)
	}
	return cert, key, []string{certPath, keyPath}, nil
}

// ensureLeaf writes a CA-signed server leaf when it's missing.
// Returns the paths written (empty when the leaf already exists).
func ensureLeaf(dir string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, sans []string) ([]string, error) {
	certPath := filepath.Join(dir, serverCertFile)
	keyPath := filepath.Join(dir, serverKeyFile)
	certThere, keyThere := fileExists(certPath), fileExists(keyPath)
	if certThere && keyThere {
		return nil, nil
	}
	// No-clobber invariant (mirrors ensureCA, Q17-certgen-1): only mint a
	// fresh leaf when NEITHER half is present. If exactly one of server.crt /
	// server.key exists, minting would overwrite the surviving half and
	// silently invalidate the distributed/served cert. Refuse instead.
	if certThere != keyThere {
		present, missing := certPath, keyPath
		if keyThere {
			present, missing = keyPath, certPath
		}
		return nil, fmt.Errorf(
			"certgen: leaf half-present: %q exists but its pair %q is missing — refusing to overwrite (restore the missing file or remove both to re-mint)",
			present, missing)
	}

	dns, ips := splitSANs(sans)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certgen: leaf key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "w17 dev.local server"},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(devCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("certgen: create leaf: %w", err)
	}
	if err := writeCertPEM(certPath, der); err != nil {
		return nil, err
	}
	if err := writeKeyPEM(keyPath, key); err != nil {
		return nil, err
	}
	return []string{certPath, keyPath}, nil
}

// splitSANs partitions sans into DNS names and IPs. "localhost" +
// loopback IPs (127.0.0.1, ::1) are ALWAYS present so a leaf is
// reachable over loopback regardless of the caller's list — dev
// dials routinely target localhost even when the service is also
// known by a compose name.
func splitSANs(sans []string) (dns []string, ips []net.IP) {
	seenDNS := map[string]bool{}
	addDNS := func(s string) {
		if !seenDNS[s] {
			seenDNS[s] = true
			dns = append(dns, s)
		}
	}
	// Mirror seenDNS for IPs, keyed on the canonical net.IP string —
	// so a caller-supplied loopback (e.g. "127.0.0.1") isn't appended
	// twice when the unconditional loopback pair is added below, and
	// duplicate SANs collapse to one IPAddresses entry.
	seenIP := map[string]bool{}
	addIP := func(ip net.IP) {
		k := ip.String()
		if !seenIP[k] {
			seenIP[k] = true
			ips = append(ips, ip)
		}
	}
	addDNS("localhost")
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			addIP(ip)
			continue
		}
		addDNS(s)
	}
	addIP(net.IPv4(127, 0, 0, 1))
	addIP(net.IPv6loopback)
	return dns, ips
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func randSerial() (*big.Int, error) {
	return randSerialFrom(rand.Reader)
}

// randSerialFrom draws a 128-bit certificate serial from r. RFC 5280
// §4.1.2.2 requires the serial to be a POSITIVE integer, but rand.Int
// returns a value in [0, 2^128) — it can yield 0. Draw from [0, 2^128)
// then add 1, shifting the range to [1, 2^128] so the serial is never 0.
func randSerialFrom(r io.Reader) (*big.Int, error) {
	serial, err := rand.Int(r, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("certgen: serial: %w", err)
	}
	return serial.Add(serial, big.NewInt(1)), nil
}

func writeCertPEM(path string, der []byte) error {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o644); err != nil {
		return fmt.Errorf("certgen: write %q: %w", path, err)
	}
	return nil
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("certgen: marshal key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return fmt.Errorf("certgen: write %q: %w", path, err)
	}
	return nil
}

// validateReusableCA guards the CA-reuse path: a CA we re-sign leaves
// with must actually BE a usable CA. It checks the cert carries the CA
// basic constraint + cert-sign key usage, is still inside its validity
// window, and that the private key matches the certificate's public key
// (a mismatched pair would mint leaves no client could verify). On any
// failure it refuses with a clear directive — this is dev-only material,
// so "delete + re-mint" is the right recovery, not silent reuse.
func validateReusableCA(cert *x509.Certificate, key *ecdsa.PrivateKey, certPath, keyPath string) error {
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf(
			"certgen: reuse CA: %q is not a signing CA (missing IsCA / KeyUsageCertSign) — delete %q + %q to re-mint",
			certPath, certPath, keyPath)
	}
	if !time.Now().Before(cert.NotAfter) {
		return fmt.Errorf(
			"certgen: reuse CA: %q expired at %s — delete %q + %q to re-mint",
			certPath, cert.NotAfter.Format(time.RFC3339), certPath, keyPath)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !key.PublicKey.Equal(pub) {
		return fmt.Errorf(
			"certgen: reuse CA: private key %q does not match certificate %q — delete both to re-mint",
			keyPath, certPath)
	}
	return nil
}

// loadCert parses a cert+key pair from disk into the structures
// CA signing needs.
func loadCert(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("%q: no PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil {
		return nil, nil, fmt.Errorf("%q: no PEM key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(kblock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}
