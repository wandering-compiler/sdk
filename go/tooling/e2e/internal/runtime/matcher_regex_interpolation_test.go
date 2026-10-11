package runtime

import (
	"strings"
	"testing"
)

func regexExpect(t *testing.T, pattern string, actual string, captures map[string]any) error {
	t.Helper()
	sc := NewRun().NewScope()
	sc.Seed(captures)
	return MatchExpect(
		map[string]any{"xml": map[string]any{"matcher": "regex", "pattern": pattern}},
		map[string]any{"xml": actual}, sc)
}

// An ISDOC round trip: `${rt_number}` in a regex
// pattern was matched as those literal characters while the same token in a
// plain expect was interpolated. It is now interpolated like everywhere else.
func TestRegexPatternInterpolatesCaptures(t *testing.T) {
	err := regexExpect(t, "<ID>${rt_number}</ID>", "<Invoice><ID>FA1442026-0001</ID></Invoice>",
		map[string]any{"rt_number": "FA1442026-0001"})
	if err != nil {
		t.Fatalf("an interpolated pattern did not match: %v", err)
	}
}

// The shape that made it dangerous: a token beside an alternation matched
// ANYTHING while uninterpolated. Interpolated, a wrong value fails.
func TestRegexPatternWithAWrongCaptureFails(t *testing.T) {
	err := regexExpect(t, "^<ID>(${rt_number})</ID>$", "<ID>FA-9</ID>",
		map[string]any{"rt_number": "FA-1"})
	if err == nil {
		t.Fatal("a pattern built from a capture matched a different value")
	}
	if !strings.Contains(err.Error(), "interpolated") || !strings.Contains(err.Error(), "FA-1") {
		t.Errorf("the failure must show the interpolated pattern: %v", err)
	}
}

// A captured value is data, not regex: its metacharacters are literal, so
// `1.2` does not match `1x2`.
func TestRegexPatternQuotesTheCapturedValue(t *testing.T) {
	if err := regexExpect(t, "^v${ver}$", "v1x2", map[string]any{"ver": "1.2"}); err == nil {
		t.Fatal("a `.` from a captured value matched any character")
	}
	if err := regexExpect(t, "^v${ver}$", "v1.2", map[string]any{"ver": "1.2"}); err != nil {
		t.Fatalf("the captured value did not match itself: %v", err)
	}
}

// A reference no step bound is an error, not an empty substitution — an
// empty `${x}` inside `.*${x}.*` would match everything.
func TestRegexPatternWithAnUnboundCaptureIsAnError(t *testing.T) {
	err := regexExpect(t, ".*${never_bound}.*", "anything", nil)
	if err == nil {
		t.Fatal("an unbound reference in a pattern passed")
	}
}

// An escaped `$` keeps meaning the characters — both spellings a regex
// author uses for a literal `${x}`, `\${x}` among them (the second
// spelling was once taken as a token, with the backslash left in front
// of the value).
func TestRegexPatternEscapedDollarBraceIsLiteral(t *testing.T) {
	for _, pat := range []string{`^\$\{x\}$`, `^\${x}$`} {
		if err := regexExpect(t, pat, "${x}", map[string]any{"x": "d42"}); err != nil {
			t.Errorf("%s: an escaped ${ was interpolated: %v", pat, err)
		}
	}
}

// A JSON number decodes as float64; fmt.Sprint wrote 1234567 as
// 1.234567e+06, so a captured numeric id never matched in a pattern.
// Integral numbers print as integers, others without an exponent.
func TestRegexPatternWithANumericCapture(t *testing.T) {
	if err := regexExpect(t, "<ID>${id}</ID>", "<ID>1234567</ID>", map[string]any{"id": float64(1234567)}); err != nil {
		t.Fatalf("a numeric capture did not match its own text: %v", err)
	}
	if err := regexExpect(t, "^${amount}$", "0.0000001", map[string]any{"amount": float64(0.0000001)}); err != nil {
		t.Fatalf("a fractional capture was written with an exponent: %v", err)
	}
}

// The same rendering in an ordinary interpolated string.
func TestEmbeddedNumericCaptureHasNoExponent(t *testing.T) {
	sc := NewRun().NewScope()
	sc.Seed(map[string]any{"id": float64(1234567)})
	got, err := sc.expandString("order-${id}")
	if err != nil || got != "order-1234567" {
		t.Fatalf("expandString = %v, %v; want order-1234567", got, err)
	}
}

// A pattern that is not a regex still fails at decode, tokens or not.
func TestRegexPatternWithATokenIsStillCheckedAtDecode(t *testing.T) {
	if _, err := DecodeMatcher(map[string]any{"matcher": "regex", "pattern": "(${x}"}); err == nil {
		t.Fatal("an unbalanced pattern with a token decoded")
	}
}
