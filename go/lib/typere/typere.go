// Package typere holds the canonical type validators the w17
// storage codegen emits for type-derived field checks — the
// precompiled regexes (UUID / SLUG / URL / MAC) plus the
// allocation-free [ValidEmail] byte-scan. One shared definition
// per type, referenced by every handler in every generated
// bundle that validates a field of that type.
//
// Why a shared package vs per-handler `var _validateUuidRe`:
//
//   - Multiple services in one Go package (e.g. eight
//     `<domain>-storage/src/mutation/<service>.go` files in
//     the same `mutation` package) would each declare an
//     identical `_validateUuidRe`, and Go rejects duplicate
//     package-level idents. Centralising the type-derived
//     regexes here lets the generator emit references rather
//     than declarations + sidesteps the collision entirely.
//   - Pattern strings are part of the W17 type contract
//     (UUID = RFC 4122 canonical shape, SLUG = lowercase
//     alphanum + dashes, …). The wire shape is identical
//     across projects and across generator versions; baking
//     them into the runtime lib makes that fixed contract
//     explicit + a single source of truth.
//
// Author-supplied `(w17.field).pattern` overrides + ad-hoc
// per-field patterns still emit as per-handler package-level
// `var`s (with the handler-scoped naming `_validate<Message>_<Field>Re`
// — never collides because the message name is the namespace).
package typere

import (
	"regexp"
	"strings"
)

// UUID matches the RFC 4122 canonical 8-4-4-4-12 hex shape.
// Generated handlers validate every `(w17.field).type = UUID`
// request field against this regex before issuing the
// downstream DQL.
var UUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidEmail reports whether s is a practically-valid email
// address. Generated handlers validate every `(w17.field).type
// = EMAIL` request field through this before the downstream DQL
// / gRPC call.
//
// Contract (pragmatic, the de-facto web-form shape):
//   - exactly one `@`, with a non-empty local part before it;
//   - a non-empty domain after it that contains a `.` and does
//     not start or end with one;
//   - no spaces or control characters anywhere.
//
// Replaces the earlier `net/mail.ParseAddress` check (perf
// sweep). ParseAddress allocated a throwaway *mail.Address on
// EVERY valid request just to return err==nil — ~5 allocs /
// 88 B / ~190ns. This byte-scan is allocation-free AND ~3.6×
// faster (~54ns, 0 allocs) because it never builds an
// intermediate object. (A regexp.MatchString alternative is
// also 0-alloc but ~5× slower than the scan for the complex
// email shape, so the hand-scan wins outright.)
//
// Semantics tighten deliberately vs ParseAddress: the bare
// scan rejects RFC 5322 display-name forms (`Foo <a@b.com>`)
// and dotless domains (`user@localhost`) that ParseAddress
// accepted — a plain email field should never have carried
// those. Common addresses (`user@example.com`) pass both.
//
// Lives here beside the type regexes (UUID/SLUG/…) because it
// serves the same role — the single canonical validator the
// codegen references for a `(w17.field).type` — even though it
// scans rather than matches a pattern.
func ValidEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at >= len(s)-1 {
		return false // missing '@', empty local, or empty domain
	}
	// Reject the space AND every control character (CR, LF, TAB, NUL,
	// DEL, …). net/mail.ParseAddress — which this scan replaced —
	// rejected them implicitly; dropping that check let a value like
	// `user@host.com\r\nBcc: …` pass, a CRLF / email-header-injection
	// vector at any downstream SMTP sink. No valid email carries a
	// byte ≤ 0x20 or == 0x7f.
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] == 0x7f {
			return false
		}
	}
	domain := s[at+1:]
	if strings.IndexByte(domain, '@') >= 0 {
		return false // more than one '@'
	}
	if domain[0] == '.' || domain[len(domain)-1] == '.' {
		return false // leading / trailing dot in domain
	}
	return strings.IndexByte(domain, '.') >= 0 // domain needs a dot
}

// Slug matches the Django-style URL slug (lowercase alphanum +
// dashes, no leading / trailing / consecutive dashes).
// `(w17.field).type = SLUG` request fields ride through this.
var Slug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// URL matches the relaxed "starts with http(s)://, has at
// least one character after" shape generated handlers apply
// to `(w17.field).type = URL`. The wire-validation is
// deliberately minimal — full RFC 3986 lives at the gateway's
// URL parsing step; here we just guard against the obvious
// "looks like garbage" cases.
var URL = regexp.MustCompile(`^https?://.+$`)

// MAC matches IEEE 802 MAC addresses in colon-or-dash-separated
// canonical form (`aa:bb:cc:dd:ee:ff` / `AA-BB-CC-DD-EE-FF`).
// `(w17.field).type = MAC_ADDRESS` request fields validate
// against this before the downstream INSERT / UPDATE.
var MAC = regexp.MustCompile(`^([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$`)
