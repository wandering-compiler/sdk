package typere

import "testing"

func TestValidEmail(t *testing.T) {
	valid := []string{
		"user@example.com",
		"a@b.co",
		"first.last@sub.example.org",
		"user+tag@example.com",
		"x@y.z.w",
	}
	for _, e := range valid {
		if !ValidEmail(e) {
			t.Errorf("ValidEmail(%q) = false, want true", e)
		}
	}

	invalid := []string{
		"",                  // empty
		"plainstring",       // no @
		"@example.com",      // empty local
		"user@",             // empty domain
		"user@localhost",    // no dot in domain
		"user@.com",         // leading dot in domain
		"user@example.",     // trailing dot in domain
		"user@@example.com", // double @
		"a b@example.com",   // space in local
		"user@exa mple.com", // space in domain
	}
	for _, e := range invalid {
		if ValidEmail(e) {
			t.Errorf("ValidEmail(%q) = true, want false", e)
		}
	}
}

// TestValidEmail_RejectsControlChars. The byte-scan
// replaced net/mail.ParseAddress, which rejected control characters;
// the scan must keep doing so. A value carrying CR/LF/TAB/NUL is never
// a valid email and, reaching an SMTP-header sink downstream, is a
// classic CRLF (header-injection) vector. The original scan rejected
// only the ASCII space and silently accepted the rest.
func TestValidEmail_RejectsControlChars(t *testing.T) {
	bad := []string{
		"user@example.com\r\nBcc: attacker@evil.com", // CRLF header injection
		"user@example.com\n",                         // bare LF
		"user@example.com\r",                         // bare CR
		"user\t@example.com",                         // TAB in local
		"user@exa\tmple.com",                         // TAB in domain
		"user@example.com\x00",                       // NUL
		"\x7fuser@example.com",                       // DEL
	}
	for _, e := range bad {
		if ValidEmail(e) {
			t.Errorf("ValidEmail(%q) = true, want false (control char must be rejected)", e)
		}
	}
}

// Unicode whitespace and control characters are as invalid as their ASCII
// kin: a no-break space, a line or paragraph separator, a C1 control or a
// trailing space must not pass; a non-ASCII letter still does.
func TestValidEmail_RejectsUnicodeSpaceAndControl(t *testing.T) {
	bad := []string{
		"user @example.com",      // NO-BREAK SPACE
		"user@example.com x",     // LINE SEPARATOR
		"user@exa mple.com",      // PARAGRAPH SEPARATOR
		"user\u0085@example.com", // NEXT LINE (C1 control)
		"user@example.com ",      // trailing space
		"　user@example.com",      // IDEOGRAPHIC SPACE
	}
	for _, e := range bad {
		if ValidEmail(e) {
			t.Errorf("ValidEmail(%q) = true, want false", e)
		}
	}
	if !ValidEmail("jiří@příklad.cz") {
		t.Error("ValidEmail rejected non-ASCII letters")
	}
}
