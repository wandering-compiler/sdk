// Package randstr generates cryptographically-safe random strings
// of caller-specified length. The storage codegen mutation-handler
// preamble calls [Token] when filling a column whose
// `(w17.field).default_auto = CRYPTO_RANDOM` and whose request
// value is empty.
//
// Design notes:
//
//   - Encoding is base64url-no-padding (RFC 4648 §5). URL-safe
//     charset means tokens survive query strings, path segments,
//     cookies, and headers without further escaping; no-padding
//     keeps the length predictable (every input byte produces
//     close to 4/3 output characters, never trailing `=`).
//
//   - The output is truncated to exactly `n` characters. The
//     truncation point falls inside a 4-character base64 group
//     for most `n` not divisible by 4, which is fine — the result
//     is still a valid sequence of base64url characters; the
//     surplus bits beyond the truncation point are simply unused.
//     Entropy retained = `6 * n` bits (each base64 char carries
//     6 bits).
//
//   - 48 random bytes → 64 base64url characters before truncation,
//     384 bits of entropy. Matches the modern industry default for
//     opaque bearer tokens (Stripe, GitHub PAT, …).
//
// Spec: docs/decisions/plugin-redesign-2026-05-22.md §CRYPTO_RANDOM.
package randstr

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Token returns an `n`-character base64url-encoded random string.
// Uses `crypto/rand` exclusively — never `math/rand`.
//
// `n` must be positive; non-positive `n` is a caller bug (the
// upstream IR validator refuses `default_auto: CRYPTO_RANDOM`
// without `max_len > 0`, so a Token(0) call indicates a codegen
// misuse).
func Token(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("randstr.Token: n must be positive, got %d", n)
	}
	// Each base64 character encodes 6 bits = 3/4 of a byte. We
	// need ceil(n * 6 / 8) bytes of entropy to fill `n` chars
	// of base64 output. Add a one-byte slack for the rounding
	// safety; the truncation at the end discards the surplus.
	byteLen := (n*6 + 7) / 8
	buf := make([]byte, byteLen)
	// coverage-exempt: Go 1.26 crypto/rand.Read never returns an
	// error — an unavailable entropy source aborts the process via
	// runtime fatal (see go.dev/issue/66821), so this branch is a
	// belt-and-suspenders guard that no test can drive.
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("randstr.Token: rand.Read: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(buf)
	// coverage-exempt: byteLen = ceil(n*6/8) guarantees
	// EncodedLen(byteLen) >= n for all positive n, so the encoded
	// string is never shorter than the request — this guard cannot
	// fire and exists only to fail loud on a future formula change.
	if len(encoded) < n {
		// Shouldn't happen given the byteLen formula, but guard
		// against off-by-one rather than silently returning a
		// shorter string than asked for.
		return "", fmt.Errorf("randstr.Token: encoded length %d < requested %d (byteLen=%d)", len(encoded), n, byteLen)
	}
	return encoded[:n], nil
}
