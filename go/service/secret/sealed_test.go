package secret

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A sealed value opens with its recipient's identity only, round-trips the
// credential exactly, and says nothing of it in its own text.
func TestSealOpen(t *testing.T) {
	id, rec, err := GenerateAgeKey()
	if err != nil {
		t.Fatal(err)
	}
	otherID, _, err := GenerateAgeKey()
	if err != nil {
		t.Fatal(err)
	}
	const token = "rst_3f9c2b7e41aa"
	sealed, err := Seal(rec, token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, token) {
		t.Fatal("the sealed value carries the credential in clear")
	}
	if again, _ := Seal(rec, token); again == sealed {
		t.Error("sealing twice gave the same text: a reader could match credentials across events")
	}
	got, err := OpenSealed(id, sealed)
	if err != nil || got != token {
		t.Fatalf("OpenSealed = %q, %v; want the token", got, err)
	}
	// A key file's content works too: comments, then the identity.
	if got, err := OpenSealed("# created: today\n# public key: "+rec+"\n"+id+"\n", sealed); err != nil || got != token {
		t.Errorf("from a key file: %q, %v", got, err)
	}
	if _, err := OpenSealed(otherID, sealed); err == nil {
		t.Error("another identity opened it")
	}
	raw, _ := base64.StdEncoding.DecodeString(sealed)
	raw[len(raw)-1] ^= 1
	if _, err := OpenSealed(id, base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Error("an altered value opened")
	}
	if _, err := OpenSealed(id, "not base64 !"); err == nil {
		t.Error("garbage opened")
	}
	if _, err := Seal("age1notarecipient", token); err == nil {
		t.Error("sealed to a malformed recipient")
	}
}

func TestCheckRecipient(t *testing.T) {
	_, rec, err := GenerateAgeKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRecipient("  " + rec + "\n"); err != nil {
		t.Errorf("a recipient with space around it: %v", err)
	}
	id, _, _ := GenerateAgeKey()
	for _, bad := range []string{"", "age1", "ssh-ed25519 AAAA", id} {
		if err := CheckRecipient(bad); err == nil {
			t.Errorf("CheckRecipient(%q) accepted it", bad)
		}
	}
}
