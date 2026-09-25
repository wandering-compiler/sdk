package fieldcrypt_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/fieldcrypt"
)

func key(b byte) string {
	raw := make([]byte, fieldcrypt.KeyBytes)
	for i := range raw {
		raw[i] = b + byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func ring(t *testing.T, spec string) *fieldcrypt.Keyring {
	t.Helper()
	kr, err := fieldcrypt.Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	return kr
}

// The default mode must not store the same plaintext the same way twice.
// Without this the database still reveals which rows hold equal values, which
// is most of what an attacker wants from a leaked dump.
func TestRandomised_TheSameSecretStoresDifferentlyEveryTime(t *testing.T) {
	kr := ring(t, "1:"+key(1))
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		got, err := kr.EncryptRandomised("hunter2")
		if err != nil {
			t.Fatal(err)
		}
		if seen[got] {
			t.Fatalf("two of 16 encryptions of one secret are identical — the column leaks equality")
		}
		seen[got] = true
		back, err := kr.Decrypt(got)
		if err != nil || back != "hunter2" {
			t.Fatalf("round trip: %q, %v", back, err)
		}
	}
}

// And the opt-in mode must, or an index over the column enforces nothing.
func TestDeterministic_TheSameSecretAlwaysStoresIdentically(t *testing.T) {
	kr := ring(t, "1:"+key(1))
	first, err := kr.EncryptDeterministic("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		again, err := kr.EncryptDeterministic("hunter2")
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("deterministic mode produced two ciphertexts for one secret — "+
				"a UNIQUE INDEX over this column would enforce nothing\n  %s\n  %s", first, again)
		}
	}
	// Different secrets must still differ, or the index collapses everything.
	other, err := kr.EncryptDeterministic("hunter3")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("two different secrets share a ciphertext")
	}
	back, err := kr.Decrypt(first)
	if err != nil || back != "hunter2" {
		t.Fatalf("round trip: %q, %v", back, err)
	}
}

// Reading back does not care which mode wrote the value. This is what lets a
// column keep being READ across a mode change — and why the migrator refuses
// that change anyway: reading is the half that keeps working while the
// uniqueness guarantee silently does not.
func TestDecrypt_IsModeAgnostic(t *testing.T) {
	kr := ring(t, "1:"+key(1))
	r, _ := kr.EncryptRandomised("s")
	d, _ := kr.EncryptDeterministic("s")
	for name, stored := range map[string]string{"randomised": r, "deterministic": d} {
		back, err := kr.Decrypt(stored)
		if err != nil || back != "s" {
			t.Errorf("%s: %q, %v", name, back, err)
		}
	}
}

// Rotation: a keyring holding both keys decrypts the old and writes the new.
// Without the version inside the value there is no gradual re-encryption and
// rotation becomes a flag day nobody schedules.
func TestRotation_OldValuesStayReadableAndNewOnesUseTheNewestKey(t *testing.T) {
	old := ring(t, "1:"+key(1))
	legacy, err := old.EncryptRandomised("s")
	if err != nil {
		t.Fatal(err)
	}

	rotated := ring(t, "1:"+key(1)+",2:"+key(9))
	if rotated.CurrentVersion() != 2 {
		t.Fatalf("current version = %d, want the highest (2)", rotated.CurrentVersion())
	}
	if back, err := rotated.Decrypt(legacy); err != nil || back != "s" {
		t.Fatalf("a value written under v1 stopped being readable after rotation: %q, %v", back, err)
	}
	fresh, err := rotated.EncryptRandomised("s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fresh, "w17c.2.") {
		t.Errorf("new value %q was not written under the newest key", fresh)
	}
}

// Retiring a key before its rows are re-encrypted must say so, not fail
// vaguely — the remedy is to put that key back until the backfill finishes.
func TestDecrypt_ARetiredKeyVersionNamesItself(t *testing.T) {
	stored, err := ring(t, "1:"+key(1)).EncryptRandomised("s")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ring(t, "2:"+key(9)).Decrypt(stored)
	if !errors.Is(err, fieldcrypt.ErrUnknownKeyVersion) {
		t.Fatalf("err = %v, want ErrUnknownKeyVersion", err)
	}
	if !strings.Contains(err.Error(), "version 1") {
		t.Errorf("the error does not name the missing version: %v", err)
	}
}

// A column converted from plaintext still holds unencrypted rows. Saying which
// problem that is beats returning gibberish or an authentication failure.
func TestDecrypt_PlaintextIsReportedAsNotEncrypted(t *testing.T) {
	kr := ring(t, "1:"+key(1))
	for _, stored := range []string{"", "plain-old-secret", "w17c.1.d.not-base64!!", "w17c.1.x.AAAA"} {
		if _, err := kr.Decrypt(stored); !errors.Is(err, fieldcrypt.ErrNotEncrypted) {
			t.Errorf("Decrypt(%q) = %v, want ErrNotEncrypted", stored, err)
		}
	}
}

// A tampered row is NOT "not encrypted" — it is ours and it does not
// authenticate. Collapsing the two would send somebody to run a data migration
// when what they have is a corrupted or re-keyed row.
func TestDecrypt_ATamperedValueIsDistinguishedFromPlaintext(t *testing.T) {
	kr := ring(t, "1:"+key(1))
	stored, _ := kr.EncryptRandomised("s")
	// Flip a byte in the MIDDLE of the payload. The LAST base64 character of a
	// raw (unpadded) encoding carries fewer than six significant bits, so
	// changing it can decode to the very same bytes — the first version of
	// this test did that and reported a tampered value as having decrypted.
	mid := len(stored) - 8
	tampered := stored[:mid] + string(flip(stored[mid])) + stored[mid+1:]

	_, err := kr.Decrypt(tampered)
	if err == nil {
		t.Fatal("a tampered value decrypted")
	}
	if errors.Is(err, fieldcrypt.ErrNotEncrypted) {
		t.Error("a tampered value was reported as unencrypted, which points at the wrong remedy")
	}
	if !strings.Contains(err.Error(), "does not authenticate") {
		t.Errorf("err = %v, want it to name the authentication failure", err)
	}
}

func flip(c byte) byte {
	if c == 'A' {
		return 'B'
	}
	return 'A'
}

// The wrong key is an authentication failure too, not silent garbage.
func TestDecrypt_AWrongKeyDoesNotSilentlyProduceGarbage(t *testing.T) {
	stored, _ := ring(t, "1:"+key(1)).EncryptRandomised("s")
	if _, err := ring(t, "1:"+key(200)).Decrypt(stored); err == nil {
		t.Fatal("a value decrypted under the wrong key")
	}
}

// Start-up refuses a configuration that cannot work, rather than deferring the
// failure to the first read on a machine nobody is watching.
func TestParse_RefusesAConfigurationThatCannotWork(t *testing.T) {
	for name, spec := range map[string]string{
		"empty":          "",
		"no version":     base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"zero version":   "0:" + key(1),
		"duplicate":      "1:" + key(1) + ",1:" + key(2),
		"not base64":     "1:@@@@",
		"wrong key size": "1:" + base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"negative":       "-1:" + key(1),
	} {
		if _, err := fieldcrypt.Parse(spec); err == nil {
			t.Errorf("%s: accepted %q", name, spec)
		}
	}
}

// The empty string is a legitimate value and must survive the round trip —
// an "optional secret that is not set yet" is a normal row.
func TestRoundTrip_TheEmptyStringIsAValue(t *testing.T) {
	kr := ring(t, "1:"+key(1))
	stored, err := kr.EncryptRandomised("")
	if err != nil {
		t.Fatal(err)
	}
	if stored == "" {
		t.Fatal("the empty string encrypted to the empty string — nothing was stored")
	}
	if back, err := kr.Decrypt(stored); err != nil || back != "" {
		t.Fatalf("round trip: %q, %v", back, err)
	}
}
