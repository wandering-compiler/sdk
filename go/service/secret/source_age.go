package secret

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// NewAgeFileSource decrypts an age-encrypted dotenv file (.secrets.age)
// with the given age identity and returns a Source over its KEY=VALUE
// contents. This is the one in-process crypto the runtime keeps: age is
// tiny, pure-Go, no cgo, no KMS — cheap enough that the "optional key"
// tier works for a bare binary with no compose/k8s orchestrator (a
// -server a solo dev runs on a VM). SOPS-with-KMS decryption stays at
// the deploy boundary; the app never carries it.
//
// keyData is the age identity text (one or more `AGE-SECRET-KEY-1…`
// lines, comments allowed) — the same key `age`/`sops` use, so the file
// is editable with the tools devops already run.
func NewAgeFileSource(path, keyData string) (Source, error) {
	ids, err := age.ParseIdentities(strings.NewReader(keyData))
	if err != nil {
		return nil, fmt.Errorf("secret: parse age identity: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("secret: no age identities found in key")
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secret: open age file %s: %w", path, err)
	}
	defer f.Close()

	dec, err := age.Decrypt(f, ids...)
	if err != nil {
		return nil, fmt.Errorf("secret: decrypt %s (wrong key?): %w", path, err)
	}
	var plain bytes.Buffer
	if _, err := io.Copy(&plain, dec); err != nil {
		return nil, fmt.Errorf("secret: read decrypted %s: %w", path, err)
	}
	return mapSource{kv: parseDotenv(plain.String())}, nil
}
