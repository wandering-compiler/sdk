package secret

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// GenerateAgeKey mints a fresh X25519 age keypair. identity is the
// private key ("AGE-SECRET-KEY-1…", kept secret, never committed);
// recipient is the public key ("age1…", committed to the lock + used
// to encrypt). Tooling (`w17ctl secrets init`) writes identity to a
// gitignored key file and records recipient in the lock.
func GenerateAgeKey() (identity, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", fmt.Errorf("secret: generate age key: %w", err)
	}
	return id.String(), id.Recipient().String(), nil
}

// EncryptToAge encrypts plaintext to age-armor-free binary for the given
// recipients ("age1…" public keys) and returns the ciphertext — the body
// of a `.secrets.age` file. Round-trips with [NewAgeFileSource] and with
// the standard `age` CLI (same format), so devops can decrypt/edit it
// with the tools they already run.
func EncryptToAge(plaintext []byte, recipients ...string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("secret: no age recipients to encrypt for")
	}
	recs := make([]age.Recipient, 0, len(recipients))
	for _, r := range recipients {
		rec, err := age.ParseX25519Recipient(strings.TrimSpace(r))
		if err != nil {
			return nil, fmt.Errorf("secret: parse recipient %q: %w", r, err)
		}
		recs = append(recs, rec)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recs...)
	if err != nil {
		return nil, fmt.Errorf("secret: age encrypt: %w", err)
	}
	if _, err := io.Copy(w, bytes.NewReader(plaintext)); err != nil {
		return nil, fmt.Errorf("secret: age write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("secret: age close: %w", err)
	}
	return buf.Bytes(), nil
}
