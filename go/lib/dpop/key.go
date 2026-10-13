package dpop

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	jose "github.com/go-jose/go-jose/v4"
)

// Signer holds a device key and signs proofs with it. Its method set is
// crypto.Signer's, so a key held by a KMS, an HSM or a platform keystore
// plugs in through any existing crypto.Signer adapter; [Key] is the in-memory
// implementation.
//
// Public must return a *ecdsa.PublicKey on P-256 (proofs use ES256) or an
// ed25519.PublicKey (EdDSA). Sign follows crypto.Signer: for P-256 it gets a
// SHA-256 digest and returns an ASN.1 signature, for Ed25519 the whole
// message with crypto.Hash(0).
type Signer interface {
	Public() crypto.PublicKey
	Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error)
}

// KeyType names the curve of a generated device key.
type KeyType string

const (
	// KeyP256 is ECDSA on P-256, signing with ES256. The default.
	KeyP256 KeyType = "P-256"
	// KeyEd25519 is Ed25519, signing with EdDSA.
	KeyEd25519 KeyType = "Ed25519"
)

// ErrUnsupportedKey is returned for a key that is neither P-256 nor Ed25519.
var ErrUnsupportedKey = errors.New("dpop: unsupported key: want ECDSA P-256 or Ed25519")

// Key is an in-memory device key. It implements [Signer].
type Key struct {
	priv crypto.Signer // *ecdsa.PrivateKey or ed25519.PrivateKey
}

// GenerateKey creates a new device key. An empty type means [KeyP256].
func GenerateKey(kt KeyType) (*Key, error) {
	switch kt {
	case "", KeyP256:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("dpop: generate P-256 key: %w", err)
		}
		return &Key{priv: k}, nil
	case KeyEd25519:
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("dpop: generate Ed25519 key: %w", err)
		}
		return &Key{priv: k}, nil
	}
	return nil, fmt.Errorf("%w: key type %q", ErrUnsupportedKey, kt)
}

// NewKey wraps an existing private key, which must be a *ecdsa.PrivateKey on
// P-256 or an ed25519.PrivateKey.
func NewKey(priv crypto.Signer) (*Key, error) {
	if _, err := algFor(priv.Public()); err != nil {
		return nil, err
	}
	switch priv.(type) {
	case *ecdsa.PrivateKey, ed25519.PrivateKey:
		return &Key{priv: priv}, nil
	}
	return nil, fmt.Errorf("%w: %T", ErrUnsupportedKey, priv)
}

// Public returns the public half of the key.
func (k *Key) Public() crypto.PublicKey { return k.priv.Public() }

// Sign signs with the private key; see [Signer].
func (k *Key) Sign(r io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	return k.priv.Sign(r, digest, opts)
}

// Thumbprint is the RFC 7638 thumbprint of the key's public half: the device
// id.
func (k *Key) Thumbprint() string {
	tp, err := Thumbprint(k.Public())
	if err != nil {
		// Unreachable: a Key only ever holds a supported key.
		panic(err)
	}
	return tp
}

// MarshalPEM encodes the private key as a PKCS #8 "PRIVATE KEY" PEM block.
func (k *Key) MarshalPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.priv)
	if err != nil {
		return nil, fmt.Errorf("dpop: marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseKeyPEM decodes a PKCS #8 "PRIVATE KEY" PEM block holding a P-256 or
// Ed25519 key.
func ParseKeyPEM(data []byte) (*Key, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("dpop: no PKCS #8 PRIVATE KEY PEM block")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("dpop: parse key: %w", err)
	}
	s, ok := priv.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedKey, priv)
	}
	return NewKey(s)
}

// LoadKeyFile reads a key written by [Key.SaveFile].
func LoadKeyFile(path string) (*Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dpop: read key: %w", err)
	}
	return ParseKeyPEM(data)
}

// SaveFile writes the key as PEM to path with mode 0600. The file is written
// next to path and renamed over it, so a reader never sees a partial key and
// an existing file with a wider mode is replaced, not reused.
func (k *Key) SaveFile(path string) (err error) {
	data, err := k.MarshalPEM()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("dpop: save key: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("dpop: save key: %w", err)
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("dpop: save key: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("dpop: save key: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("dpop: save key: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("dpop: save key: %w", err)
	}
	return nil
}

// Thumbprint returns the RFC 7638 JWK SHA-256 thumbprint of a public key,
// base64url without padding. It accepts any key type go-jose can express as a
// JWK (so the RFC's RSA example is checkable), but a device key is always
// P-256 or Ed25519.
func Thumbprint(pub crypto.PublicKey) (string, error) {
	jwk := jose.JSONWebKey{Key: pub}
	if !jwk.Valid() || !jwk.IsPublic() {
		return "", fmt.Errorf("%w: %T", ErrUnsupportedKey, pub)
	}
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("dpop: thumbprint: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

// AccessTokenHash is the `ath` value for an access token: base64url (no
// padding) of its SHA-256.
func AccessTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// algFor picks the JWS algorithm for a device public key.
func algFor(pub crypto.PublicKey) (jose.SignatureAlgorithm, error) {
	switch p := pub.(type) {
	case *ecdsa.PublicKey:
		if p.Curve == elliptic.P256() {
			return jose.ES256, nil
		}
	case ed25519.PublicKey:
		if len(p) == ed25519.PublicKeySize {
			return jose.EdDSA, nil
		}
	}
	return "", fmt.Errorf("%w: %T", ErrUnsupportedKey, pub)
}
