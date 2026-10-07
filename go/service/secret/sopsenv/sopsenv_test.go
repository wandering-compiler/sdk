package sopsenv

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func keypair(t *testing.T) (*age.X25519Identity, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id, id.Recipient().String()
}

const plain = "# a comment\nDB_PASSWORD=s3cr3t\nEMPTY=\nURL=\"postgres://a:b@h:5432/d?x=1&y=2\"\nNOTE_unencrypted=visible\nMULTI=one\\ntwo\n"

func TestRoundTrip(t *testing.T) {
	id, rec := keypair(t)
	lines, err := Parse([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := Encrypt(lines, []string{rec}, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s := string(enc)
	for _, want := range []string{"#ENC[AES256_GCM,", "DB_PASSWORD=ENC[AES256_GCM,", "EMPTY=\n", "NOTE_unencrypted=visible\n",
		"sops_age__list_0__map_recipient=" + rec, "sops_lastmodified=2026-10-05T12:00:00Z", "sops_unencrypted_suffix=_unencrypted", `-----BEGIN AGE ENCRYPTED FILE-----\n`} {
		if !strings.Contains(s, want) {
			t.Errorf("sealed file lacks %q\n%s", want, s)
		}
	}
	if strings.Contains(s, "s3cr3t") {
		t.Fatal("a value is in clear")
	}
	got, err := Decrypt(enc, []age.Identity{id})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Format(got), []byte(plain)) {
		t.Errorf("round trip changed the file:\n%s\nwant\n%s", Format(got), plain)
	}
}

// Every change to the sealed body is caught: a value, a key name (it is the
// additional data), the MAC's timestamp.
func TestDecrypt_RefusesTampering(t *testing.T) {
	id, rec := keypair(t)
	lines, _ := Parse([]byte(plain))
	enc, _ := Encrypt(lines, []string{rec}, time.Now())
	s, _ := Read(enc)
	var swapped string
	for _, l := range s.Body {
		if l.Key == "DB_PASSWORD" {
			swapped = l.Value
		}
	}
	for name, tamper := range map[string]func(string) string{
		"value moved to another key": func(f string) string {
			return strings.Replace(f, "URL=", "URL_OLD=", 1)
		},
		"value replaced": func(f string) string {
			return strings.Replace(f, "NOTE_unencrypted=visible", "NOTE_unencrypted=changed", 1)
		},
		"line dropped": func(f string) string {
			return strings.Replace(f, "DB_PASSWORD="+swapped+"\n", "", 1)
		},
		"timestamp changed": func(f string) string {
			re := strings.Index(f, "sops_lastmodified=")
			return f[:re] + "sops_lastmodified=2000-01-01T00:00:00Z" + f[strings.Index(f[re:], "\n")+re:]
		},
	} {
		if _, err := Decrypt([]byte(tamper(string(enc))), []age.Identity{id}); err == nil {
			t.Errorf("%s: decrypted anyway", name)
		}
	}
	other, _ := keypair(t)
	if _, err := Decrypt(enc, []age.Identity{other}); err == nil || !strings.Contains(err.Error(), rec) {
		t.Errorf("a wrong key must be refused, naming the recipients: %v", err)
	}
}

func TestRekey_KeepsValuesOpensForTheNewKeyOnly(t *testing.T) {
	oldID, oldRec := keypair(t)
	newID, newRec := keypair(t)
	lines, _ := Parse([]byte(plain))
	enc, _ := Encrypt(lines, []string{oldRec}, time.Now())
	re, err := Rekey(enc, []age.Identity{oldID}, []string{newRec})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Read(enc)
	b, _ := Read(re)
	if !equalLines(a.Body, b.Body) || a.Meta["sops_mac"] != b.Meta["sops_mac"] {
		t.Error("rekey changed the values or the MAC")
	}
	if got, err := Decrypt(re, []age.Identity{newID}); err != nil || !bytes.Equal(Format(got), []byte(plain)) {
		t.Errorf("new key: %v", err)
	}
	if _, err := Decrypt(re, []age.Identity{oldID}); err == nil {
		t.Error("the removed key still opens the file")
	}
	if r := b.Recipients(); len(r) != 1 || r[0] != newRec {
		t.Errorf("recipients = %v", r)
	}
}

func TestEncrypt_RefusesReservedKeys(t *testing.T) {
	_, rec := keypair(t)
	if _, err := Encrypt([]Line{{Key: "sops_mac", Value: "x"}}, []string{rec}, time.Now()); err == nil {
		t.Error("a sops_ key was sealed")
	}
}

func equalLines(a, b []Line) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
