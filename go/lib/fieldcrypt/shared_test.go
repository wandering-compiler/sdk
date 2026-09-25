package fieldcrypt_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/fieldcrypt"
)

// An uninitialised keyring must say what was not done, not fail vaguely.
// Generated code calls these helpers from deep inside a query; the error it
// produces is the only thing an operator will see.
func TestShared_UninitialisedNamesTheOmission(t *testing.T) {
	fieldcrypt.SetShared(nil)
	for name, call := range map[string]func() (string, error){
		"decrypt":      func() (string, error) { return fieldcrypt.DecryptShared("w17c.1.r.AAAA") },
		"encrypt rand": func() (string, error) { return fieldcrypt.EncryptSharedRandomised("s") },
		"encrypt det":  func() (string, error) { return fieldcrypt.EncryptSharedDeterministic("s") },
	} {
		_, err := call()
		if err == nil {
			t.Fatalf("%s: an uninitialised keyring was used", name)
		}
		if !strings.Contains(err.Error(), "never initialised") || !strings.Contains(err.Error(), fieldcrypt.EnvKeys) {
			t.Errorf("%s: err = %v, want it to name the omission and the variable", name, err)
		}
	}
}

func TestShared_RoundTripsOnceInstalled(t *testing.T) {
	raw := make([]byte, fieldcrypt.KeyBytes)
	for i := range raw {
		raw[i] = byte(i)
	}
	kr, err := fieldcrypt.Parse("1:" + base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	fieldcrypt.SetShared(kr)
	t.Cleanup(func() { fieldcrypt.SetShared(nil) })

	for name, enc := range map[string]func(string) (string, error){
		"randomised":    fieldcrypt.EncryptSharedRandomised,
		"deterministic": fieldcrypt.EncryptSharedDeterministic,
	} {
		stored, err := enc("s3cret")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		back, err := fieldcrypt.DecryptShared(stored)
		if err != nil || back != "s3cret" {
			t.Errorf("%s: round trip = %q, %v", name, back, err)
		}
	}
}
