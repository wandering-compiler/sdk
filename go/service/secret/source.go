package secret

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Source resolves a declared secret by its env-var key. Lookup returns
// (value, true) on a hit and ("", false) when this source has no value
// for key. Sources never fail at lookup time — any real error (a corrupt
// file, a bad decryption key) surfaces when the Source is constructed,
// so boot fails loudly rather than a secret silently resolving empty.
type Source interface {
	Lookup(key string) (string, bool)
}

// Resolver is an ordered chain of Sources. The first Source that has a
// value for a key wins. It is the seam the generated bundle resolves
// every declared secret through at boot; the non-secret env surface
// keeps reading os.Getenv directly.
//
// The chain encodes the "seamless dev / strong prod" property: injected
// env always wins (a k8s Secret / ESO / Vault-agent / sops exec-env /
// plain compose value), then an encrypted .secrets.age when an age key
// is configured, then a plain .secrets for frictionless local dev. No
// step is ever required — absence degrades to the next source, presence
// upgrades, with no change to the app code or the developer's mental
// model.
type Resolver struct {
	sources []Source
}

// Lookup walks the chain; first hit wins.
func (r *Resolver) Lookup(key string) (string, bool) {
	for _, s := range r.sources {
		if v, ok := s.Lookup(key); ok {
			return v, true
		}
	}
	return "", false
}

// Get is the convenience the generated EnvconfigFromEnv calls: the
// resolved value, or "" when no source has it (an empty secret is the
// safe default). Wrap the result in [New] to keep it redacting.
func (r *Resolver) Get(key string) string {
	v, _ := r.Lookup(key)
	return v
}

// resolverConfig is the env-derived input to [NewResolver]. Zero value
// is the dev default (auto mode, conventional paths). Generated main
// fills it from W17_SECRETS_* env so deploys can override paths without
// rebuilding.
type resolverConfig struct {
	// Mode is W17_SECRETS_MODE: "auto" (default), "plain", or "age".
	// auto = env → age (when a key is present) → plain. plain = env →
	// plain (age ignored even if a key is set). age = env → age, and
	// the age file+key are REQUIRED (a strict prod posture; a missing
	// key/file is an error, not a silent downgrade).
	Mode string
	// SecretsFile is the plain dotenv path (W17_SECRETS_FILE, default
	// ".secrets").
	SecretsFile string
	// AgeFile is the age-encrypted dotenv path (W17_SECRETS_AGE_FILE,
	// default ".secrets.age").
	AgeFile string
	// AgeKey is the literal age identity (W17_SECRETS_AGE_KEY), e.g.
	// "AGE-SECRET-KEY-1…". Takes precedence over AgeKeyFile.
	AgeKey string
	// AgeKeyFile is a path to a file holding the age identity
	// (W17_SECRETS_AGE_KEY_FILE) — the conventional way to mount the
	// key (k8s Secret volume, SOPS_AGE_KEY_FILE-style).
	AgeKeyFile string
}

// configFromEnv reads the W17_SECRETS_* knobs into a resolverConfig,
// applying the conventional defaults.
func configFromEnv() resolverConfig {
	c := resolverConfig{
		Mode:        envOr("W17_SECRETS_MODE", "auto"),
		SecretsFile: envOr("W17_SECRETS_FILE", ".secrets"),
		AgeFile:     envOr("W17_SECRETS_AGE_FILE", ".secrets.age"),
		AgeKey:      os.Getenv("W17_SECRETS_AGE_KEY"),
		AgeKeyFile:  os.Getenv("W17_SECRETS_AGE_KEY_FILE"),
	}
	// Interop: honour the de-facto SOPS_AGE_KEY[_FILE] when our branded
	// keys are unset, so a host already configured for `sops`/`age`
	// Just Works.
	if c.AgeKey == "" {
		c.AgeKey = os.Getenv("SOPS_AGE_KEY")
	}
	if c.AgeKeyFile == "" {
		c.AgeKeyFile = os.Getenv("SOPS_AGE_KEY_FILE")
	}
	return c
}

// NewResolver builds the secret resolution chain from the environment.
// Convenience for the common case: newResolverFrom(configFromEnv()).
func NewResolver() (*Resolver, error) {
	return newResolverFrom(configFromEnv())
}

// newResolverFrom builds the chain from an explicit config. Returns an
// error only on misconfiguration — a corrupt/undecryptable age file, or
// mode "age" without a usable key/file. A frictionless dev box (no key,
// no encrypted file) yields a chain of {env, plain} and never errors.
func newResolverFrom(c resolverConfig) (*Resolver, error) {
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	if mode == "" {
		mode = "auto"
	}

	r := &Resolver{sources: []Source{envSource{}}}

	keyData, haveKey, err := loadAgeKey(c)
	if err != nil {
		return nil, err
	}

	switch mode {
	case "plain":
		if err := r.appendPlain(c.SecretsFile); err != nil {
			return nil, err
		}
	case "age":
		// Strict: the encrypted file + key are mandatory. A missing
		// piece is a misconfig, not a silent fall-through to plaintext.
		if !haveKey {
			return nil, fmt.Errorf("secret: mode=age but no age key (set W17_SECRETS_AGE_KEY or W17_SECRETS_AGE_KEY_FILE)")
		}
		ages, err := NewAgeFileSource(c.AgeFile, keyData)
		if err != nil {
			return nil, err
		}
		r.sources = append(r.sources, ages)
	case "auto":
		// Encrypted-when-configured: include age only when BOTH a key
		// and the encrypted file are present; otherwise degrade to
		// plain. Age wins over plain so a provisioned encrypted file is
		// never shadowed by a stray dev .secrets.
		if haveKey && fileExists(c.AgeFile) {
			ages, err := NewAgeFileSource(c.AgeFile, keyData)
			if err != nil {
				return nil, err
			}
			r.sources = append(r.sources, ages)
		}
		if err := r.appendPlain(c.SecretsFile); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("secret: unknown W17_SECRETS_MODE %q (auto|plain|age)", c.Mode)
	}
	return r, nil
}

// appendPlain adds a PlainFileSource for path when the file exists; a
// missing .secrets is normal (secrets injected via env), not an error.
// But a file that EXISTS yet cannot be read or parsed (bad perms,
// malformed content) is a real error: swallowing it would let the
// service boot with secrets silently resolving empty — blank DB
// password / API key — and fail confusingly later. Fail loud at boot
// instead (the missing-file fast path above keeps the common case
// silent).
func (r *Resolver) appendPlain(path string) error {
	if path == "" || !fileExists(path) {
		return nil
	}
	s, err := newPlainFileSource(path)
	if err != nil {
		return fmt.Errorf("secret: read plain secrets file %q: %w", path, err)
	}
	r.sources = append(r.sources, s)
	return nil
}

// envSource resolves from the process environment — the universal
// contract every deploy-layer materialiser (k8s Secret, ESO,
// Vault-agent, sops exec-env, plain compose) lands a secret in.
type envSource struct{}

func (envSource) Lookup(key string) (string, bool) { return os.LookupEnv(key) }

// mapSource is a pre-loaded key→value source backing both the plain and
// age file sources (the file is parsed/decrypted once at construction).
type mapSource struct{ kv map[string]string }

func (m mapSource) Lookup(key string) (string, bool) { v, ok := m.kv[key]; return v, ok }

// newPlainFileSource parses a plain dotenv .secrets file.
func newPlainFileSource(path string) (Source, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secret: read %s: %w", path, err)
	}
	return mapSource{kv: parseDotenv(string(b))}, nil
}

// parseDotenv parses KEY=VALUE lines. Blank lines and `#` comments are
// skipped; the split is on the first `=` so values may contain `=`. No
// quote processing — values are taken literally (matches compose
// env_file semantics), only a trailing CR (CRLF files) is trimmed.
func parseDotenv(s string) map[string]string {
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		val := strings.TrimSuffix(line[eq+1:], "\r")
		kv[key] = val
	}
	return kv
}

// loadAgeKey resolves the age identity from config: literal AgeKey wins,
// else AgeKeyFile is read. Returns (data, have, err). A missing file is
// an error (the operator pointed at a key that isn't there); no config
// at all is (nil, false, nil).
func loadAgeKey(c resolverConfig) (string, bool, error) {
	if strings.TrimSpace(c.AgeKey) != "" {
		return c.AgeKey, true, nil
	}
	if strings.TrimSpace(c.AgeKeyFile) != "" {
		b, err := os.ReadFile(c.AgeKeyFile)
		if err != nil {
			return "", false, fmt.Errorf("secret: read age key file %s: %w", c.AgeKeyFile, err)
		}
		return string(b), true, nil
	}
	return "", false, nil
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
