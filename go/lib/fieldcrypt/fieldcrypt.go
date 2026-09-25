// Package fieldcrypt encrypts CRYPTED_SECRET column values.
//
// The generated storage layer calls it on the way into the database and on the
// way out; a handler never sees ciphertext and never chooses a mode. What it
// buys is narrow and worth stating plainly: an attacker holding the DATABASE
// and not the key gets nothing. It does not defend against a compromised
// application server, which holds both.
//
// # Two modes, chosen by the schema and not by the caller
//
// A column declared `unique` is encrypted DETERMINISTICALLY — the same
// plaintext always produces the same ciphertext, which is what lets an index
// enforce anything and `WHERE col = :x` find anything. Everything else is
// RANDOMISED, where the same plaintext stores differently every time and the
// database does not reveal even which two rows match.
//
// Deterministic mode leaks equality. On a high-entropy secret that is nothing;
// on a low-entropy column it is a frequency table. The schema decides, because
// the schema is where somebody already wrote down that they need to compare
// the values — see docs/specs/storage/crypted-secret-field.md.
//
// # The stored form
//
//	w17c.<key-version>.<mode>.<base64url(nonce || ciphertext)>
//
// The version is what makes rotation possible: a keyring holds several keys at
// once, decrypts with whichever version a value names, and encrypts with the
// newest. Without it in the value there is no way to re-encrypt a table
// gradually, and rotation becomes a flag day nobody schedules.
//
// The mode travels with the value too. That is why a column whose mode changed
// still decrypts row by row — and why the migrator refuses the change anyway:
// reading keeps working while the uniqueness guarantee silently does not.
package fieldcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// EnvKeys is the environment variable the keyring is built from.
//
// Its own variable, and not a reuse of anything else the deployment already
// has. Django's SECRET_KEY serves ten purposes at once and they now recommend
// splitting it; there is no reason to arrive at the same place on purpose.
const EnvKeys = "W17_FIELD_KEYS"

// prefix marks a value this package produced. A column that was converted from
// plaintext still holds rows without it, and saying so beats returning
// gibberish — see ErrNotEncrypted.
const prefix = "w17c"

const (
	modeRandomised    = "r"
	modeDeterministic = "d"
)

// KeyBytes is the raw key length every configured key must have: AES-256.
const KeyBytes = 32

// ErrNotEncrypted is returned for a stored value that this package did not
// write. The likely cause is a column that held plaintext before it became a
// CRYPTED_SECRET, which is a data migration nobody ran.
var ErrNotEncrypted = errors.New("fieldcrypt: value is not encrypted")

// ErrUnknownKeyVersion is returned when a value names a key the keyring does
// not hold. During a rotation this means a retired key was dropped before its
// rows were re-encrypted; the fix is to put it back until they are.
var ErrUnknownKeyVersion = errors.New("fieldcrypt: no key for the version this value names")

// Keyring holds every key a deployment can decrypt with, and the one it
// encrypts with.
type Keyring struct {
	current int
	keys    map[int]*keyset
}

type keyset struct {
	aead cipher.AEAD
	// mac derives the DETERMINISTIC nonce from the plaintext. A separate
	// subkey, because deriving it with the encryption key would be reusing one
	// key for two jobs inside a single construction.
	mac []byte
}

// FromEnv builds the keyring from W17_FIELD_KEYS.
//
//	W17_FIELD_KEYS=1:<base64 32 bytes>,2:<base64 32 bytes>
//
// The HIGHEST version is the one new values are encrypted with; every listed
// version can still be decrypted. Rotation is therefore: append a key, deploy,
// re-encrypt in the background, drop the old entry once nothing names it.
//
// A service with a CRYPTED_SECRET column and no keys must REFUSE TO BOOT
// rather than start and fail on the first read — an error hours later on a
// machine nobody is watching is the same outage with the cause removed. This
// function returns an error; calling it during start-up is the contract.
func FromEnv() (*Keyring, error) {
	return Parse(os.Getenv(EnvKeys))
}

// Parse builds a keyring from the W17_FIELD_KEYS syntax.
func Parse(spec string) (*Keyring, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("fieldcrypt: %s is empty — a project with a CRYPTED_SECRET column "+
			"cannot read or write it without a key, so this is refused at start-up rather than at "+
			"the first query (set %s=1:<base64 of %d random bytes>)", EnvKeys, EnvKeys, KeyBytes)
	}
	kr := &Keyring{keys: map[int]*keyset{}}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		verStr, b64, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("fieldcrypt: %s entry %q is not <version>:<base64 key>", EnvKeys, entry)
		}
		ver, err := strconv.Atoi(strings.TrimSpace(verStr))
		if err != nil || ver <= 0 {
			return nil, fmt.Errorf("fieldcrypt: %s entry %q has a non-positive version", EnvKeys, entry)
		}
		if _, dup := kr.keys[ver]; dup {
			return nil, fmt.Errorf("fieldcrypt: %s lists version %d twice", EnvKeys, ver)
		}
		raw, err := decodeKey(strings.TrimSpace(b64))
		if err != nil {
			return nil, fmt.Errorf("fieldcrypt: %s version %d: %w", EnvKeys, ver, err)
		}
		ks, err := newKeyset(raw)
		if err != nil {
			return nil, fmt.Errorf("fieldcrypt: %s version %d: %w", EnvKeys, ver, err)
		}
		kr.keys[ver] = ks
		if ver > kr.current {
			kr.current = ver
		}
	}
	if len(kr.keys) == 0 {
		return nil, fmt.Errorf("fieldcrypt: %s listed no usable keys", EnvKeys)
	}
	return kr, nil
}

func decodeKey(b64 string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err := enc.DecodeString(b64); err == nil {
			if len(raw) != KeyBytes {
				return nil, fmt.Errorf("key is %d bytes, want %d (AES-256)", len(raw), KeyBytes)
			}
			return raw, nil
		}
	}
	return nil, errors.New("key is not valid base64")
}

// newKeyset derives the two subkeys this construction needs.
//
// HKDF with distinct info strings rather than using the configured bytes
// directly: one secret drives two different primitives here, and separating
// them is what keeps the nonce derivation from leaking anything about the
// encryption key.
func newKeyset(raw []byte) (*keyset, error) {
	encKey, err := hkdf.Key(sha256.New, raw, nil, "w17 fieldcrypt aes-256-gcm", KeyBytes)
	if err != nil {
		return nil, err
	}
	macKey, err := hkdf.Key(sha256.New, raw, nil, "w17 fieldcrypt synthetic nonce", KeyBytes)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &keyset{aead: aead, mac: macKey}, nil
}

// CurrentVersion is the key version new values are written under.
func (k *Keyring) CurrentVersion() int { return k.current }

// EncryptRandomised is the default mode: a fresh nonce per call, so the same
// plaintext never stores the same way twice.
func (k *Keyring) EncryptRandomised(plain string) (string, error) {
	ks := k.keys[k.current]
	nonce := make([]byte, ks.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("fieldcrypt: nonce: %w", err)
	}
	return k.seal(ks, modeRandomised, nonce, plain), nil
}

// EncryptDeterministic derives the nonce FROM the plaintext, so the same input
// always produces the same output and an index over the column means something.
//
// The nonce is an HMAC of the plaintext under a subkey that is not the
// encryption key — the SIV shape. Two different plaintexts colliding would
// require an HMAC-SHA256 collision, so the GCM rule that a nonce must never
// repeat for two different messages holds for the same reason the hash does.
func (k *Keyring) EncryptDeterministic(plain string) (string, error) {
	ks := k.keys[k.current]
	mac := hmac.New(sha256.New, ks.mac)
	mac.Write([]byte(plain))
	nonce := mac.Sum(nil)[:ks.aead.NonceSize()]
	return k.seal(ks, modeDeterministic, nonce, plain), nil
}

func (k *Keyring) seal(ks *keyset, mode string, nonce []byte, plain string) string {
	sealed := ks.aead.Seal(nil, nonce, []byte(plain), nil)
	body := make([]byte, 0, len(nonce)+len(sealed))
	body = append(body, nonce...)
	body = append(body, sealed...)
	return strings.Join([]string{prefix, strconv.Itoa(k.current), mode,
		base64.RawURLEncoding.EncodeToString(body)}, ".")
}

// Decrypt reads a value back, whichever mode and key version wrote it.
//
// Mode-agnostic on purpose: the nonce is stored, so opening does not care how
// it was chosen. That is what lets a column keep being READ after its mode
// changed — and exactly why the migrator refuses that change anyway, since
// reading is the half that keeps working.
func (k *Keyring) Decrypt(stored string) (string, error) {
	parts := strings.SplitN(stored, ".", 4)
	if len(parts) != 4 || parts[0] != prefix {
		return "", ErrNotEncrypted
	}
	ver, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", ErrNotEncrypted
	}
	ks, ok := k.keys[ver]
	if !ok {
		return "", fmt.Errorf("%w: version %d (keyring holds %s)", ErrUnknownKeyVersion, ver, k.versions())
	}
	if parts[2] != modeRandomised && parts[2] != modeDeterministic {
		return "", ErrNotEncrypted
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", ErrNotEncrypted
	}
	ns := ks.aead.NonceSize()
	if len(body) < ns {
		return "", ErrNotEncrypted
	}
	plain, err := ks.aead.Open(nil, body[:ns], body[ns:], nil)
	if err != nil {
		// Authentication failure: the right key version, the wrong key, or a
		// tampered row. Not ErrNotEncrypted — this value IS ours.
		return "", fmt.Errorf("fieldcrypt: value does not authenticate under key version %d "+
			"(wrong key material, or the row was altered)", ver)
	}
	return string(plain), nil
}

func (k *Keyring) versions() string {
	out := make([]string, 0, len(k.keys))
	for v := range k.keys {
		out = append(out, strconv.Itoa(v))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
