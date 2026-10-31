package secret

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// maxSealedBytes bounds what OpenSealed reads: a sealed value carries one
// short credential (a reset link's token, a mailed code), never a document.
const maxSealedBytes = 64 << 10

// Seal encrypts plaintext to one age recipient ("age1…", an X25519 public
// key) and answers it as standard base64 — the form a plugin puts on an event
// in place of a credential, so the event bus, its dead-letter queue and its
// backups carry ciphertext only. Only the holder of the matching identity
// (OpenSealed) reads it.
func Seal(recipient, plaintext string) (string, error) {
	ct, err := EncryptToAge([]byte(plaintext), recipient)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

// OpenSealed decrypts a value Seal made, with the age identity
// ("AGE-SECRET-KEY-1…", or a key file's content: comments and several
// identities are fine) of the recipient it was sealed to. A value sealed to
// another recipient, altered, or not base64 is an error, never a guess.
func OpenSealed(identity, sealed string) (string, error) {
	ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sealed))
	if err != nil {
		return "", fmt.Errorf("secret: sealed value is not base64: %w", err)
	}
	ids, err := age.ParseIdentities(strings.NewReader(identity))
	if err != nil {
		return "", fmt.Errorf("secret: parse age identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(ct), ids...)
	if err != nil {
		return "", fmt.Errorf("secret: open sealed value: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(r, maxSealedBytes+1))
	if err != nil {
		return "", fmt.Errorf("secret: read sealed value: %w", err)
	}
	if len(out) > maxSealedBytes {
		return "", fmt.Errorf("secret: sealed value is larger than %d bytes", maxSealedBytes)
	}
	return string(out), nil
}

// CheckRecipient reports whether r is an age X25519 recipient ("age1…"), so a
// configuration that names a recipient can be refused at boot rather than at
// the first credential it fails to seal.
func CheckRecipient(r string) error {
	if _, err := age.ParseX25519Recipient(strings.TrimSpace(r)); err != nil {
		return fmt.Errorf("secret: %q is not an age recipient (age1…): %w", r, err)
	}
	return nil
}
