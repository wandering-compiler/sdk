// Package dsnredact masks the credentials a connection string can carry, so
// an error or log line that has to name the DSN it is about does not also
// print the password.
//
// It is deliberately textual rather than a URL round-trip: the strings it is
// handed are often the ones that FAILED to parse (a misspelt scheme, a stray
// character), and those are exactly the ones that end up in an error message.
package dsnredact

import (
	"errors"
	"net/url"
	"regexp"
)

// Mask is what a redacted secret is replaced with.
const Mask = "xxxxx"

var (
	// `scheme://user:secret@host` — everything between the first `:` after
	// the user and the LAST `@` is treated as the password. Greedy on
	// purpose: a password containing a literal `@` must not leave its tail
	// behind, and masking a few extra characters of a broken DSN costs
	// nothing.
	userinfo = regexp.MustCompile(`(://[^:/@\s]*:)[^\s]*@`)
	// `nats://secret@host` — NATS reads a userinfo with no `:` as a TOKEN,
	// the whole credential, where every other scheme here has a username
	// that is not a secret. The production console's bus DSN has this
	// shape, and a failed connect printed it whole into the log
	// (2026-10-22).
	natsToken = regexp.MustCompile(`((?i:nats|tls)://)[^:/@\s]+@`)
	// `password=secret` in a libpq keyword DSN or a URL query, and the
	// other parameter names drivers use for a secret.
	keyword = regexp.MustCompile(`(?i)\b(password|passwd|pwd|pass|sslpassword|secret|secret_key|secret_access_key|access_token|token|auth|authtoken)=('[^']*'|[^&\s]*)`)
)

// String returns s with every credential it recognises replaced by [Mask].
// Strings with no credentials come back unchanged.
func String(s string) string {
	s = userinfo.ReplaceAllString(s, "${1}"+Mask+"@")
	s = natsToken.ReplaceAllString(s, "${1}"+Mask+"@")
	return keyword.ReplaceAllString(s, "${1}="+Mask)
}

// Err redacts the URL a *url.Error carries, in place, and returns err. The
// standard library quotes the whole input in that error, password included,
// and every DSN parse in this tree surfaces it.
func Err(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = String(ue.URL)
	}
	return err
}
