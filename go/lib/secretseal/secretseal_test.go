package secretseal

import (
	"bytes"
	"errors"
	"testing"
)

func TestSeal_OpensOnlyWithTheGrantAndTheAddress(t *testing.T) {
	priv, pub, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(pub, "g1", "web", "SMTP_PASSWORD", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("s3cret")) {
		t.Fatal("the sealed value carries the plaintext")
	}
	got, err := Open(priv, "g1", "web", "SMTP_PASSWORD", sealed)
	if err != nil || string(got) != "s3cret" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	other, _, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	for name, c := range map[string]struct {
		priv                []byte
		grant, service, key string
		sealed              []byte
	}{
		"another grant's key": {other, "g1", "web", "SMTP_PASSWORD", sealed},
		"another grant id":    {priv, "g2", "web", "SMTP_PASSWORD", sealed},
		"another service":     {priv, "g1", "api", "SMTP_PASSWORD", sealed},
		"another key":         {priv, "g1", "web", "DB_PASSWORD", sealed},
		"a tampered value":    {priv, "g1", "web", "SMTP_PASSWORD", tampered},
		"a truncated value":   {priv, "g1", "web", "SMTP_PASSWORD", sealed[:10]},
	} {
		if _, err := Open(c.priv, c.grant, c.service, c.key, c.sealed); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: err = %v, want ErrOpen", name, err)
		}
	}
}

// Two seals of one value differ: the encapsulated key is fresh each time, so
// an observer cannot tell that two writes carried the same secret.
func TestSeal_IsRandomized(t *testing.T) {
	_, pub, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Seal(pub, "g", "s", "K", []byte("v"))
	b, _ := Seal(pub, "g", "s", "K", []byte("v"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one value are identical")
	}
}

func TestSeal_RefusesABadPublicKey(t *testing.T) {
	if _, err := Seal([]byte("short"), "g", "s", "K", []byte("v")); err == nil {
		t.Fatal("a malformed public key sealed")
	}
}
