// Package secret provides a redacting value type for sensitive
// configuration — DB passwords, API tokens, signing keys, DSNs.
//
// The whole point is to make a leak *impossible by construction*
// rather than *avoided by discipline*. A [Secret] wraps a string
// whose cleartext is reachable ONLY through [Secret.Reveal]. Every
// other path a value normally escapes through — fmt verbs, the
// Stringer/GoStringer interfaces, JSON/text marshalling, log/slog —
// is overridden to emit the redaction sentinel [Redacted] ("***").
//
// So this is safe:
//
//	cfg := EnvConfig{APIKey: secret.New("sk_live_abc123")}
//	log.Printf("%+v", cfg)        // {APIKey:***}
//	slog.Info("boot", "cfg", cfg) // cfg={APIKey:***}
//	json.Marshal(cfg)             // {"APIKey":"***"}
//	fmt.Sprintf("%v %s %q %#v", cfg.APIKey, cfg.APIKey, cfg.APIKey, cfg.APIKey)
//	                              // *** *** *** ***
//
// and the one place the secret is actually used spells out the
// reveal, which keeps "where does this secret get used in cleartext"
// greppable and review-visible:
//
//	stripe.New(cfg.APIKey.Reveal())
//
// A secret can still be *loaded* from a config file (UnmarshalJSON /
// UnmarshalText populate the value) without ever becoming printable.
package secret

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// Redacted is the sentinel every stringification path emits in place
// of the wrapped value.
const Redacted = "***"

// Secret wraps a sensitive value so it cannot leak through the usual
// stringification, serialization, or logging paths. The zero Secret
// is a valid empty secret (Reveal returns "").
//
// Constrained to ~string because every real secret env var (password,
// token, key, DSN) is a string; secret int/bool is not a use case.
type Secret[T ~string] struct {
	v T
}

// String is the alias the codegen names for the common case.
type String = Secret[string]

// New wraps v in a Secret.
func New[T ~string](v T) Secret[T] { return Secret[T]{v: v} }

// Reveal returns the wrapped cleartext value. This is the ONLY way to
// read it — every other access path redacts. Spell it out at each use
// site so secret usage stays auditable.
func (s Secret[T]) Reveal() T { return s.v }

// IsZero reports whether the wrapped value is empty.
func (s Secret[T]) IsZero() bool { return s.v == "" }

// Format implements [fmt.Formatter] so EVERY fmt verb (%v, %s, %q,
// %#v, %d, …) renders [Redacted]. This is the bulletproof layer:
// fmt consults Formatter before Stringer/GoStringer, so no format
// directive can reach the wrapped value.
func (s Secret[T]) Format(f fmt.State, _ rune) { io.WriteString(f, Redacted) }

// String implements [fmt.Stringer] for non-fmt callers (a value used
// directly as a fmt.Stringer, or log/slog without LogValue support).
func (s Secret[T]) String() string { return Redacted }

// GoString implements [fmt.GoStringer] so %#v outside Format also
// redacts.
func (s Secret[T]) GoString() string { return Redacted }

// MarshalText redacts for any encoding/text consumer (YAML, env
// dumps, url.Values, …).
func (s Secret[T]) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalJSON redacts for encoding/json and JSON structured loggers.
func (s Secret[T]) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// LogValue implements [slog.LogValuer] so log/slog renders the
// redaction sentinel even when the handler reflects struct fields.
func (s Secret[T]) LogValue() slog.Value { return slog.StringValue(Redacted) }

// UnmarshalText loads a secret from a text config source. The value
// is stored but stays unprintable.
func (s *Secret[T]) UnmarshalText(b []byte) error {
	s.v = T(b)
	return nil
}

// UnmarshalJSON loads a secret from a JSON string. The value is
// stored but stays unprintable.
func (s *Secret[T]) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	s.v = T(str)
	return nil
}
