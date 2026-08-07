package secret

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestParseDotenv(t *testing.T) {
	kv := parseDotenv("# comment\n\nA=1\nB = two words \nC=has=equals\n   \n#X=skip\nD=\n")
	want := map[string]string{"A": "1", "B": " two words ", "C": "has=equals", "D": ""}
	if len(kv) != len(want) {
		t.Fatalf("kv = %v, want %v", kv, want)
	}
	for k, v := range want {
		if kv[k] != v {
			t.Errorf("kv[%q] = %q, want %q", k, kv[k], v)
		}
	}
	if _, ok := kv["X"]; ok {
		t.Errorf("commented key X should be skipped: %v", kv)
	}
}

func TestResolver_EnvWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".secrets"), "K=from_file\nONLY_FILE=f\n")
	t.Setenv("K", "from_env")

	r, err := newResolverFrom(resolverConfig{Mode: "auto", SecretsFile: filepath.Join(dir, ".secrets")})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Get("K"); got != "from_env" {
		t.Errorf("K = %q, want from_env (env wins over file)", got)
	}
	if got := r.Get("ONLY_FILE"); got != "f" {
		t.Errorf("ONLY_FILE = %q, want f", got)
	}
	if _, ok := r.Lookup("MISSING"); ok {
		t.Errorf("MISSING should not resolve")
	}
}

// No key, no encrypted file → chain is {env, plain}, never errors.
func TestResolver_AutoNoKey(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".secrets"), "DB_PASS=hunter2\n")
	r, err := newResolverFrom(resolverConfig{
		Mode:        "auto",
		SecretsFile: filepath.Join(dir, ".secrets"),
		AgeFile:     filepath.Join(dir, ".secrets.age"), // absent
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Get("DB_PASS"); got != "hunter2" {
		t.Errorf("DB_PASS = %q, want hunter2", got)
	}
}

// Strict age mode without a key is a loud misconfig, not a silent
// downgrade to plaintext.
func TestResolver_AgeModeRequiresKey(t *testing.T) {
	_, err := newResolverFrom(resolverConfig{Mode: "age", AgeFile: "x.age"})
	if err == nil {
		t.Fatal("mode=age without a key must error")
	}
}

func TestResolver_UnknownMode(t *testing.T) {
	if _, err := newResolverFrom(resolverConfig{Mode: "wat"}); err == nil {
		t.Fatal("unknown mode must error")
	}
}

// A plain secrets file that EXISTS but cannot be read must fail the
// resolver loud at boot, not be silently dropped (which would let the
// service start with secrets resolving empty). Skipped under root,
// which bypasses file permissions.
func TestResolver_PlainFileUnreadable_FailsLoud(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".secrets")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := newResolverFrom(resolverConfig{Mode: "plain", SecretsFile: path}); err == nil {
		t.Fatal("newResolverFrom silently ignored an unreadable secrets file; want a loud error")
	}
}

// Full age round-trip: generate a key, encrypt a dotenv, and prove the
// resolver decrypts it — and that the encrypted file wins over a stray
// plain .secrets (the prod-over-dev precedence).
func TestResolver_AgeRoundTripAndPrecedence(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ageFile := filepath.Join(dir, ".secrets.age")
	encryptAge(t, ageFile, id.Recipient(), "STRIPE_KEY=sk_live_real\nSHARED=from_age\n")

	plainFile := filepath.Join(dir, ".secrets")
	writeFile(t, plainFile, "SHARED=from_plain\nDEV_ONLY=d\n")

	r, err := newResolverFrom(resolverConfig{
		Mode:        "auto",
		SecretsFile: plainFile,
		AgeFile:     ageFile,
		AgeKey:      id.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Get("STRIPE_KEY"); got != "sk_live_real" {
		t.Errorf("STRIPE_KEY = %q, want sk_live_real (decrypted)", got)
	}
	if got := r.Get("SHARED"); got != "from_age" {
		t.Errorf("SHARED = %q, want from_age (age wins over plain)", got)
	}
	if got := r.Get("DEV_ONLY"); got != "d" {
		t.Errorf("DEV_ONLY = %q, want d (plain still consulted)", got)
	}
}

// A wrong key surfaces as a build-time error, not a silent empty secret.
func TestAgeFileSource_WrongKeyErrors(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	other, _ := age.GenerateX25519Identity()
	dir := t.TempDir()
	ageFile := filepath.Join(dir, ".secrets.age")
	encryptAge(t, ageFile, id.Recipient(), "K=v\n")

	if _, err := NewAgeFileSource(ageFile, other.String()); err == nil {
		t.Fatal("decrypt with the wrong key must error")
	}
}

func TestConfigFromEnv_SOPSInterop(t *testing.T) {
	t.Setenv("W17_SECRETS_AGE_KEY", "")
	t.Setenv("SOPS_AGE_KEY", "AGE-SECRET-KEY-1example")
	c := configFromEnv()
	if c.AgeKey != "AGE-SECRET-KEY-1example" {
		t.Errorf("SOPS_AGE_KEY not honoured: %q", c.AgeKey)
	}
	if c.Mode != "auto" {
		t.Errorf("default mode = %q, want auto", c.Mode)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func encryptAge(t *testing.T, path string, r age.Recipient, body string) {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, buf.String())
}
