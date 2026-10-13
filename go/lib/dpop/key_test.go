package dpop_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/dpop"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// RFC 7638 §3.1: the example RSA key's SHA-256 thumbprint.
func TestThumbprint_RFC7638Vector(t *testing.T) {
	n := "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAt" +
		"VT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn6" +
		"4tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FD" +
		"W2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n9" +
		"1CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINH" +
		"aQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw"
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(b64(t, n)), E: 65537}
	got, err := dpop.Thumbprint(pub)
	if err != nil {
		t.Fatal(err)
	}
	if want := "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"; got != want {
		t.Fatalf("thumbprint = %s, want %s", got, want)
	}
}

// rfc9449Key is the P-256 key of the RFC 9449 examples.
func rfc9449Key(t *testing.T) *ecdsa.PublicKey {
	t.Helper()
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(b64(t, "l8tFrhx-34tV3hRICRDY9zCkDlpBhF42UQUfWVAWBFs")),
		Y:     new(big.Int).SetBytes(b64(t, "9VE4jf_Ok_o64zbTTlcuNJajHmt6v9TDVrU0CdvGRDA")),
	}
}

// RFC 9449 §6.1: the example key's jkt.
func TestThumbprint_RFC9449KeyMatchesJKT(t *testing.T) {
	got, err := dpop.Thumbprint(rfc9449Key(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I"; got != want {
		t.Fatalf("jkt = %s, want %s", got, want)
	}
}

// RFC 9449 §7.1: the example access token's ath.
func TestAccessTokenHash_RFC9449Vector(t *testing.T) {
	got := dpop.AccessTokenHash("Kz~8mXK1EalYznwH-LC-1fBAo.4Ljp~zsPE_NeO.gxU")
	if want := "fUHyO2r2Z3DZ53EsNrWBb0xWXoaNy59IiKCAqksmQEo"; got != want {
		t.Fatalf("ath = %s, want %s", got, want)
	}
}

func TestKey_GenerateBothTypes(t *testing.T) {
	for _, kt := range []dpop.KeyType{"", dpop.KeyP256, dpop.KeyEd25519} {
		k, err := dpop.GenerateKey(kt)
		if err != nil {
			t.Fatalf("%q: %v", kt, err)
		}
		v := newVerifier(t, nil)
		proof, err := dpop.NewProof(k, "GET", testOrigin+"/x", "", "")
		if err != nil {
			t.Fatalf("%q: proof: %v", kt, err)
		}
		tp, err := v.Verify(context.Background(), dpop.Request{Method: "GET", Path: "/x", Proofs: []string{proof}})
		if err != nil {
			t.Fatalf("%q: verify: %v", kt, err)
		}
		if tp != k.Thumbprint() {
			t.Fatalf("%q: thumbprint %s, key says %s", kt, tp, k.Thumbprint())
		}
	}
	if _, err := dpop.GenerateKey("P-384"); !errors.Is(err, dpop.ErrUnsupportedKey) {
		t.Fatalf("P-384 = %v, want ErrUnsupportedKey", err)
	}
}

func TestKey_UnsupportedKeysRefused(t *testing.T) {
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, s := range []crypto.Signer{p384, rsaKey} {
		if _, err := dpop.NewKey(s); !errors.Is(err, dpop.ErrUnsupportedKey) {
			t.Fatalf("NewKey(%T) = %v, want ErrUnsupportedKey", s, err)
		}
		if _, err := dpop.NewProof(s, "GET", testOrigin+"/", "", ""); !errors.Is(err, dpop.ErrUnsupportedKey) {
			t.Fatalf("NewProof(%T) = %v, want ErrUnsupportedKey", s, err)
		}
	}
}

func TestKey_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, kt := range []dpop.KeyType{dpop.KeyP256, dpop.KeyEd25519} {
		k, err := dpop.GenerateKey(kt)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, string(kt)+".pem")
		// A pre-existing world-readable file is replaced, not reused.
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := k.SaveFile(path); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := st.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s: mode %o, want 600", kt, mode)
		}
		back, err := dpop.LoadKeyFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if back.Thumbprint() != k.Thumbprint() {
			t.Fatalf("%s: loaded key differs", kt)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	if _, err := dpop.ParseKeyPEM([]byte("not pem")); err == nil {
		t.Fatal("ParseKeyPEM accepted garbage")
	}
}

// opaqueSigner exposes nothing but crypto.Signer's methods, the way a KMS or
// HSM adapter does.
type opaqueSigner struct{ inner crypto.Signer }

func (o opaqueSigner) Public() crypto.PublicKey { return o.inner.Public() }
func (o opaqueSigner) Sign(r io.Reader, d []byte, opts crypto.SignerOpts) ([]byte, error) {
	return o.inner.Sign(r, d, opts)
}

func TestSigner_ExternalCryptoSigner(t *testing.T) {
	for _, kt := range []dpop.KeyType{dpop.KeyP256, dpop.KeyEd25519} {
		k, _ := dpop.GenerateKey(kt)
		s := opaqueSigner{inner: k}
		proof, err := dpop.NewProof(s, "POST", testOrigin+"/p", "tok", "")
		if err != nil {
			t.Fatalf("%s: %v", kt, err)
		}
		v := newVerifier(t, nil)
		tp, err := v.Verify(context.Background(), dpop.Request{
			Method: "POST", Path: "/p", Proofs: []string{proof}, AccessToken: "tok", Binding: k.Thumbprint(),
		})
		if err != nil || tp != k.Thumbprint() {
			t.Fatalf("%s: verify = %q, %v", kt, tp, err)
		}
	}
}
