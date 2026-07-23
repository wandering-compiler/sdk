package certgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// seedValidCA returns the bytes of a real CA cert + key minted by a
// throwaway EnsureCerts run, so reuse-path tests can hand loadCert a
// genuinely-parseable cert while corrupting only the partner file.
func seedValidCA(t *testing.T) (caCrt, caKey []byte) {
	t.Helper()
	seed := t.TempDir()
	if _, err := EnsureCerts(seed, []string{"localhost"}); err != nil {
		t.Fatalf("seed EnsureCerts: %v", err)
	}
	c, err := os.ReadFile(filepath.Join(seed, caFile))
	if err != nil {
		t.Fatal(err)
	}
	k, err := os.ReadFile(filepath.Join(seed, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return c, k
}

// EnsureCerts fails fast when the target dir can't be created
// because a path component is a regular file (MkdirAll error).
func TestEnsureCerts_MkdirAllError(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "iamafile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "file/sub" — MkdirAll can't make a dir under a regular file.
	if _, err := EnsureCerts(filepath.Join(file, "sub"), nil); err == nil {
		t.Error("expected MkdirAll error, got nil")
	}
}

// EnsureCerts fails at the very first write — the flock(2) sentinel
// — when the target dir is read-only. This genuinely exercises the
// lockDir error return: with nothing pre-created, os.OpenFile can't
// create .certgen.lock under a 0o500 dir. (The write-arm tests below
// deliberately pre-create the sentinel so they reach PAST this point.)
func TestEnsureCerts_LockDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir is not enforced for root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := EnsureCerts(dir, nil)
	if err == nil {
		t.Fatal("expected lockDir error on read-only dir, got nil")
	}
	if !strings.Contains(err.Error(), lockFile) {
		t.Errorf("error should come from the %s sentinel open; got: %v", lockFile, err)
	}
}

// EnsureCerts surfaces the .gitignore write error. To actually REACH
// the gitignore write in a read-only dir the run must first clear
// lockDir, so the .certgen.lock sentinel is pre-created (an existing
// file opens RDWR fine even in a 0o500 dir); .gitignore is left
// absent so the write branch is taken and then fails.
func TestEnsureCerts_GitignoreWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir is not enforced for root")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := EnsureCerts(dir, nil)
	if err == nil {
		t.Fatal("expected gitignore write error on read-only dir, got nil")
	}
	if !strings.Contains(err.Error(), gitignoreFile) {
		t.Errorf("error should come from the %s write; got: %v", gitignoreFile, err)
	}
}

// NOTE: the former TestEnsureCerts_{CACert,CAKey,LeafCert,LeafKey}WriteError
// tests were retired. As written they chmod'd the dir read-only with no
// sentinel, so they tripped lockDir long before the write arm they named
// (they only ever asserted a loose err != nil). Their intended arms are
// already covered: the CA/leaf *cert*-write arms by
// TestEnsureCerts_{CA,Leaf}MintWriteError (coverage_extra_test.go), the
// writeCertPEM/writeKeyPEM error arms directly by
// TestWrite{Cert,Key}PEM_WriteError (coverage_extra_test.go), and the
// cert/key half-present refusals by TestEnsure{CA,Leaf}_HalfPresent_*
// (certgen_fix_test.go).

// EnsureCerts refuses to reuse a CA whose on-disk material is
// corrupt — loadCert's failure modes must propagate, not produce a
// silently-broken leaf. Table covers every loadCert error branch.
func TestEnsureCerts_CorruptCAReuse(t *testing.T) {
	validCrt, validKey := seedValidCA(t)
	badDERCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-a-der")})
	badDERKey := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not-a-der")})

	cases := []struct {
		name   string
		crt    []byte // nil → write a directory instead (unreadable)
		key    []byte // nil → write a directory instead (unreadable)
		crtDir bool
		keyDir bool
	}{
		{name: "cert-not-pem", crt: []byte("garbage, no pem"), key: validKey},
		{name: "cert-bad-der", crt: badDERCert, key: validKey},
		{name: "cert-unreadable-dir", key: validKey, crtDir: true},
		{name: "key-not-pem", crt: validCrt, key: []byte("garbage, no pem")},
		{name: "key-bad-der", crt: validCrt, key: badDERKey},
		{name: "key-unreadable-dir", crt: validCrt, keyDir: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			crtPath := filepath.Join(dir, caFile)
			keyPath := filepath.Join(dir, caKeyFile)
			if c.crtDir {
				if err := os.Mkdir(crtPath, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(crtPath, c.crt, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if c.keyDir {
				if err := os.Mkdir(keyPath, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(keyPath, c.key, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := EnsureCerts(dir, []string{"localhost"}); err == nil {
				t.Errorf("%s: expected reuse error, got nil", c.name)
			}
		})
	}
}

// loadCert round-trips a freshly-minted CA pair back into parsed
// structures — the happy path the reuse branch relies on.
func TestLoadCert_HappyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, []string{"localhost"}); err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}
	cert, key, err := loadCert(filepath.Join(dir, caFile), filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatalf("loadCert: %v", err)
	}
	if cert == nil || key == nil {
		t.Fatal("loadCert returned nil cert/key on valid material")
	}
	if !cert.IsCA {
		t.Error("loaded CA cert should have IsCA=true")
	}
}

// writeKeyPEM / writeCertPEM accept a freshly-generated key and DER
// and emit decodable PEM (sanity around the write helpers used above).
func TestWriteHelpers_EmitDecodablePEM(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "k.pem")
	if err := writeKeyPEM(keyPath, key); err != nil {
		t.Fatalf("writeKeyPEM: %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if block, _ := pem.Decode(raw); block == nil || block.Type != "EC PRIVATE KEY" {
		t.Errorf("writeKeyPEM emitted non-decodable PEM: %q", raw)
	}
	// 0o600 perms on the private key (skip the check on Windows).
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("key perms = %v, want 0600", info.Mode().Perm())
		}
	}
}
