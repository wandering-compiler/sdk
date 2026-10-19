// Package secretseal seals one environment value to a console write grant,
// so a secret leaves the machine that set it only as ciphertext only the
// console can open (decision secrets-env-and-bot-tokens-in-the-console,
// "Sealed writes").
//
// The grant is an HPKE key pair (RFC 9180: DHKEM(X25519), HKDF-SHA256,
// AES-256-GCM) the console mints for one project, one environment and five
// minutes. Its private half never leaves the console; the CLI gets the public
// half and seals every value to it. TLS already protects the wire; this
// protects the value from everything that reads a request outside TLS — a
// gateway log, a proxy, a browser extension or bug reporter on the page.
//
// Each value is sealed to its ADDRESS: the grant, the service and the key are
// the HPKE info, so a sealed value opens only as the value of the key it was
// sealed for. A captured one cannot be replayed under another key, and the
// console ends a grant after its one use, so not under the same key either.
package secretseal

import (
	"crypto/ecdh"
	"crypto/hpke"
	"errors"
	"fmt"
)

// The suite, fixed: both ends must agree, and a negotiable suite is one more
// thing an attacker gets to choose.
var (
	kem  = hpke.DHKEM(ecdh.X25519())
	kdf  = hpke.HKDFSHA256()
	aead = hpke.AES256GCM()
)

// version prefixes the info, so a later suite cannot open what this one
// sealed (or the reverse) by accident.
const version = "w17 secretseal v1"

// NewKeyPair mints a grant's key pair: the private key (the console keeps it,
// encrypted at rest, for the grant's lifetime) and the public key (handed to
// the CLI that seals).
func NewKeyPair() (private, public []byte, err error) {
	k, err := kem.GenerateKey()
	if err != nil {
		return nil, nil, fmt.Errorf("secretseal: generate key: %w", err)
	}
	private, err = k.Bytes()
	if err != nil {
		return nil, nil, fmt.Errorf("secretseal: serialize key: %w", err)
	}
	return private, k.PublicKey().Bytes(), nil
}

// Seal encrypts value to the grant's public key, bound to the grant, the
// service and the key it is the value of.
func Seal(public []byte, grantID, service, key string, value []byte) ([]byte, error) {
	pk, err := kem.NewPublicKey(public)
	if err != nil {
		return nil, fmt.Errorf("secretseal: the grant's public key: %w", err)
	}
	out, err := hpke.Seal(pk, kdf, aead, info(grantID, service, key), value)
	if err != nil {
		return nil, fmt.Errorf("secretseal: seal: %w", err)
	}
	return out, nil
}

// ErrOpen is every failure to open: a wrong key, a value sealed for another
// address or another grant, a tampered ciphertext. One error on purpose — the
// caller has nothing to do differently between them.
var ErrOpen = errors.New("secretseal: the sealed value does not open with this grant for this key")

// Open decrypts what Seal produced, with the grant's private key and the same
// address.
func Open(private []byte, grantID, service, key string, sealed []byte) ([]byte, error) {
	k, err := kem.NewPrivateKey(private)
	if err != nil {
		return nil, fmt.Errorf("secretseal: the grant's private key: %w", err)
	}
	out, err := hpke.Open(k, kdf, aead, info(grantID, service, key), sealed)
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}

// info is the address a value is sealed to. NUL-separated: a NUL cannot occur
// in a grant id, a service name or an env var name, so no two addresses share
// an info.
func info(grantID, service, key string) []byte {
	return []byte(version + "\x00" + grantID + "\x00" + service + "\x00" + key)
}
