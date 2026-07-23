package secret

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- keygen.go ---

// TestGenerateAndEncryptRoundTrip mints a fresh keypair, encrypts a dotenv
// body to the recipient, and proves the resolver decrypts it with the
// identity — covering GenerateAgeKey + EncryptToAge end-to-end.
func TestGenerateAndEncryptRoundTrip(t *testing.T) {
	identity, recipient, err := GenerateAgeKey()
	if err != nil {
		t.Fatalf("GenerateAgeKey: %v", err)
	}
	if identity == "" || recipient == "" {
		t.Fatal("empty identity/recipient")
	}
	ct, err := EncryptToAge([]byte("API_KEY=sk_live_xyz\n"), recipient)
	if err != nil {
		t.Fatalf("EncryptToAge: %v", err)
	}
	ageFile := filepath.Join(t.TempDir(), ".secrets.age")
	writeFile(t, ageFile, string(ct))
	src, err := NewAgeFileSource(ageFile, identity)
	if err != nil {
		t.Fatalf("NewAgeFileSource: %v", err)
	}
	if v, ok := src.Lookup("API_KEY"); !ok || v != "sk_live_xyz" {
		t.Errorf("decrypted lookup = %q,%v", v, ok)
	}
}

func TestEncryptToAge_NoRecipients(t *testing.T) {
	if _, err := EncryptToAge([]byte("x")); err == nil {
		t.Fatal("want error with no recipients")
	}
}

func TestEncryptToAge_BadRecipient(t *testing.T) {
	if _, err := EncryptToAge([]byte("x"), "not-an-age-recipient"); err == nil {
		t.Fatal("want parse error for a malformed recipient")
	}
}

// --- source.go: NewResolver / loadAgeKey / age mode / helpers ---

// TestNewResolver_DevDefaults exercises NewResolver()→configFromEnv() with a
// clean env and a cwd that has no .secrets — a frictionless chain of
// {env, plain-absent} that never errors.
func TestNewResolver_DevDefaults(t *testing.T) {
	// Clear every knob so configFromEnv yields pure defaults.
	for _, k := range []string{
		"W17_SECRETS_MODE", "W17_SECRETS_FILE", "W17_SECRETS_AGE_FILE",
		"W17_SECRETS_AGE_KEY", "W17_SECRETS_AGE_KEY_FILE",
		"SOPS_AGE_KEY", "SOPS_AGE_KEY_FILE",
	} {
		t.Setenv(k, "")
	}
	// Run from an empty cwd so the default ".secrets"/".secrets.age" are absent.
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver()
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	t.Setenv("FROM_ENV", "v")
	if got := r.Get("FROM_ENV"); got != "v" {
		t.Errorf("env lookup = %q, want v", got)
	}
}

// TestResolver_AgeMode_KeyFromFile covers loadAgeKey's AgeKeyFile branch and
// the strict age-mode happy path.
func TestResolver_AgeMode_KeyFromFile(t *testing.T) {
	identity, recipient, err := GenerateAgeKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ct, err := EncryptToAge([]byte("DB_PASS=hunter2\n"), recipient)
	if err != nil {
		t.Fatal(err)
	}
	ageFile := filepath.Join(dir, ".secrets.age")
	writeFile(t, ageFile, string(ct))
	keyFile := filepath.Join(dir, "key.txt")
	writeFile(t, keyFile, identity+"\n")

	r, err := newResolverFrom(resolverConfig{Mode: "age", AgeFile: ageFile, AgeKeyFile: keyFile})
	if err != nil {
		t.Fatalf("newResolverFrom: %v", err)
	}
	if got := r.Get("DB_PASS"); got != "hunter2" {
		t.Errorf("DB_PASS = %q, want hunter2", got)
	}
}

// TestLoadAgeKey_MissingKeyFile — pointing at a key file that isn't there is a
// loud error, not a silent (nil,false).
func TestLoadAgeKey_MissingKeyFile(t *testing.T) {
	_, _, err := loadAgeKey(resolverConfig{AgeKeyFile: filepath.Join(t.TempDir(), "nope")})
	if err == nil {
		t.Fatal("want error for missing age key file")
	}
}

// TestResolver_AgeMode_BadAgeFile — mode=age with a key but an unreadable /
// missing encrypted file surfaces NewAgeFileSource's error.
func TestResolver_AgeMode_BadAgeFile(t *testing.T) {
	identity, _, err := GenerateAgeKey()
	if err != nil {
		t.Fatal(err)
	}
	_, err = newResolverFrom(resolverConfig{
		Mode:    "age",
		AgeFile: filepath.Join(t.TempDir(), "absent.age"),
		AgeKey:  identity,
	})
	if err == nil {
		t.Fatal("want error opening a missing age file in mode=age")
	}
}

// TestResolver_AutoMode_CorruptAgeFile — auto mode with a key and a present
// but corrupt .secrets.age surfaces the decrypt error (no silent downgrade).
func TestResolver_AutoMode_CorruptAgeFile(t *testing.T) {
	identity, _, err := GenerateAgeKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ageFile := filepath.Join(dir, ".secrets.age")
	writeFile(t, ageFile, "this is not a valid age file\n")
	_, err = newResolverFrom(resolverConfig{Mode: "auto", AgeFile: ageFile, AgeKey: identity})
	if err == nil {
		t.Fatal("want decrypt error for a corrupt age file in auto mode")
	}
}

// TestResolver_PlainMode_EmptyFile — Mode=plain with an empty SecretsFile path
// short-circuits appendPlain (path=="") and yields just {env}.
func TestResolver_PlainMode_EmptyFile(t *testing.T) {
	r, err := newResolverFrom(resolverConfig{Mode: "plain", SecretsFile: ""})
	if err != nil {
		t.Fatalf("newResolverFrom: %v", err)
	}
	if _, ok := r.Lookup("ANYTHING"); ok {
		t.Error("nothing should resolve from an env-only chain")
	}
}

// TestResolver_EmptyModeDefaultsAuto — an empty Mode normalises to "auto".
func TestResolver_EmptyModeDefaultsAuto(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".secrets"), "K=v\n")
	r, err := newResolverFrom(resolverConfig{Mode: "", SecretsFile: filepath.Join(dir, ".secrets")})
	if err != nil {
		t.Fatalf("newResolverFrom: %v", err)
	}
	if got := r.Get("K"); got != "v" {
		t.Errorf("K = %q, want v (empty mode → auto → plain consulted)", got)
	}
}

// TestParseDotenv_EdgeLines covers the no-`=` and empty-key skip arms.
func TestParseDotenv_EdgeLines(t *testing.T) {
	kv := parseDotenv("noequalsline\n=valueWithoutKey\nGOOD=ok\n")
	if len(kv) != 1 || kv["GOOD"] != "ok" {
		t.Errorf("kv = %v, want only GOOD=ok", kv)
	}
}

func TestFileExists(t *testing.T) {
	if fileExists("") {
		t.Error("empty path should not exist")
	}
	dir := t.TempDir()
	if !fileExists(filepathJoinFile(t, dir)) {
		t.Error("written file should exist")
	}
	if fileExists(dir) {
		t.Error("a directory must not count as a file")
	}
}

func filepathJoinFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "f")
	writeFile(t, p, "x")
	return p
}

func TestEnvOr(t *testing.T) {
	t.Setenv("W17_TEST_ENVOR", "")
	if got := envOr("W17_TEST_ENVOR", "def"); got != "def" {
		t.Errorf("unset → %q, want def", got)
	}
	t.Setenv("W17_TEST_ENVOR", "set")
	if got := envOr("W17_TEST_ENVOR", "def"); got != "set" {
		t.Errorf("set → %q, want set", got)
	}
}

// --- source_age.go ---

func TestNewAgeFileSource_NoIdentities(t *testing.T) {
	// A comment-only key has zero identities.
	if _, err := NewAgeFileSource("whatever", "# just a comment\n"); err == nil {
		t.Fatal("want error when key has no identities")
	}
}

func TestNewAgeFileSource_MalformedIdentity(t *testing.T) {
	// A malformed AGE-SECRET-KEY line fails ParseIdentities (vs. zero ids).
	if _, err := NewAgeFileSource("whatever", "AGE-SECRET-KEY-1NOTVALID\n"); err == nil {
		t.Fatal("want parse error for a malformed age identity")
	}
}

func TestNewAgeFileSource_MissingFile(t *testing.T) {
	identity, _, err := GenerateAgeKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAgeFileSource(filepath.Join(t.TempDir(), "absent.age"), identity); err == nil {
		t.Fatal("want open error for a missing age file")
	}
}

// --- secret.go ---

func TestSecret_UnmarshalJSON_Error(t *testing.T) {
	var s String
	if err := s.UnmarshalJSON([]byte("not json")); err == nil {
		t.Fatal("want error for invalid JSON")
	}
	// Valid round-trip stays redacted.
	if err := json.Unmarshal([]byte(`"sk_live"`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Reveal() != "sk_live" {
		t.Errorf("reveal = %q", s.Reveal())
	}
}
