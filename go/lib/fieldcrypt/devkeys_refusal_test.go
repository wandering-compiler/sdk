package fieldcrypt_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/fieldcrypt"
)

// The public development key boots only in the development environment —
// enforced by Init itself, not left to whoever calls it.
func TestInit_RefusesTheDevelopmentKeyOutsideDev(t *testing.T) {
	t.Cleanup(func() { fieldcrypt.SetShared(nil) })
	real, err := fieldcrypt.NewKeySpec("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		keys, env string
		refused   bool
	}{
		{fieldcrypt.DevKeys, "prod", true},
		{fieldcrypt.DevKeys, "", true},
		{fieldcrypt.DevKeys, "staging", true},
		{real + "," + strings.Replace(fieldcrypt.DevKeys, "1:", "2:", 1), "prod", true}, // under another version, next to a real key
		{fieldcrypt.DevKeys, "dev", false},
		{real, "prod", false},
	} {
		t.Setenv(fieldcrypt.EnvKeys, tc.keys)
		t.Setenv(fieldcrypt.EnvEnvironment, tc.env)
		err := fieldcrypt.Init()
		if tc.refused != errors.Is(err, fieldcrypt.ErrDevKeys) {
			t.Errorf("W17_ENV=%q, dev key present=%v: Init() = %v", tc.env, strings.Contains(tc.keys, fieldcrypt.DevKeys[2:]), err)
		}
		if tc.refused {
			if _, encErr := fieldcrypt.EncryptSharedRandomised("x"); encErr == nil {
				t.Errorf("W17_ENV=%q: a refused keyring was installed anyway", tc.env)
			}
		}
	}
}
