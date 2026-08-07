package certgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

// TestRandSerialFrom_ReaderError covers the rand.Int error arm.
func TestRandSerialFrom_ReaderError(t *testing.T) {
	if _, err := randSerialFrom(failReader{}); err == nil {
		t.Fatal("want error when the entropy source fails")
	}
}

// TestWriteCertPEM_WriteError covers writeCertPEM's os.WriteFile error arm via
// a path whose parent is a regular file.
func TestWriteCertPEM_WriteError(t *testing.T) {
	base := t.TempDir()
	fileSlot := filepath.Join(base, "afile")
	if err := os.WriteFile(fileSlot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeCertPEM(filepath.Join(fileSlot, "sub", "ca.crt"), []byte("der")); err == nil {
		t.Fatal("want write error when the parent path is a file")
	}
}

// TestEnsureCerts_CAMintWriteError reaches ensureCA's mint→writeCertPEM error
// arm: pre-creating the lock sentinel + .gitignore lets lockDir and the
// gitignore step succeed in a read-only dir, so the failure lands on the CA
// cert write (the existing read-only-dir tests trip on lockDir first).
func TestEnsureCerts_CAMintWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir is not enforced for root")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, gitignoreFile), []byte(gitignoreBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := EnsureCerts(dir, nil); err == nil {
		t.Fatal("want CA mint write error in a read-only dir")
	}
}

// TestEnsureCerts_LeafMintWriteError reaches ensureLeaf's mint→write error arm:
// mint a full PKI, drop the leaf, make the dir read-only (CA + sentinel +
// gitignore all still present), then re-ensure → CA reused, leaf write fails.
func TestEnsureCerts_LeafMintWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir is not enforced for root")
	}
	dir := t.TempDir()
	if _, err := EnsureCerts(dir, nil); err != nil {
		t.Fatalf("initial EnsureCerts: %v", err)
	}
	for _, f := range []string{serverCertFile, serverKeyFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := EnsureCerts(dir, nil); err == nil {
		t.Fatal("want leaf mint write error in a read-only dir")
	}
}

// TestWriteKeyPEM_WriteError covers writeKeyPEM's os.WriteFile error arm with a
// valid key (so marshalling succeeds and only the write fails).
func TestWriteKeyPEM_WriteError(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	fileSlot := filepath.Join(base, "afile")
	if err := os.WriteFile(fileSlot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeKeyPEM(filepath.Join(fileSlot, "sub", "ca.key"), key); err == nil {
		t.Fatal("want write error when the parent path is a file")
	}
}
