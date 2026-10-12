package typere

import (
	"regexp"
	"testing"
)

// TestTypeRegexes_MatchTheirDocumentedShapes pins each shared validator
// against the shape its doc names, with near-miss decoys on both sides.
func TestTypeRegexes_MatchTheirDocumentedShapes(t *testing.T) {
	cases := []struct {
		name string
		re   *regexp.Regexp
		ok   []string
		bad  []string
	}{
		{"UUID", UUID,
			[]string{
				"123e4567-e89b-12d3-9456-426614174000", // v1
				"9F1C2B7E-4D3A-4F6B-8C9D-0E1F2A3B4C5D", // v4, upper case
				"01890a5d-ac96-774b-bcce-b302099a8057", // v7
				"00000000-0000-0000-0000-000000000000", // nil UUID
			},
			[]string{
				"123e4567e89b12d3a456426614174000",      // no dashes
				"123e4567-e89b-12d3-9456-42661417400",   // short last group
				"123e4567-e89b-12d3-9456-4266141740000", // long last group
				"g23e4567-e89b-12d3-9456-426614174000",  // non-hex
				"{123e4567-e89b-12d3-9456-426614174000}",
				" 123e4567-e89b-12d3-9456-426614174000",
				"",
			}},
		{"Slug", Slug,
			[]string{"a", "abc", "a-b", "post-2024-recap", "0"},
			[]string{"", "A", "a_b", "-a", "a-", "a--b", "a b", "á"}},
		{"URL", URL,
			[]string{"http://x", "https://example.com/p?q=1", "https://a"},
			[]string{"", "http://", "https://", "ftp://x", "example.com", " https://x", "HTTP://x"}},
		{"MAC", MAC,
			[]string{"aa:bb:cc:dd:ee:ff", "AA-BB-CC-DD-EE-FF", "01:23:45:67:89:ab"},
			[]string{"", "aabbccddeeff", "aa:bb:cc:dd:ee", "aa:bb:cc:dd:ee:ff:00", "gg:bb:cc:dd:ee:ff", "aa.bb.cc.dd.ee.ff", "a:bb:cc:dd:ee:ff"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range c.ok {
				if !c.re.MatchString(s) {
					t.Errorf("%q rejected, want accepted", s)
				}
			}
			for _, s := range c.bad {
				if c.re.MatchString(s) {
					t.Errorf("%q accepted, want rejected", s)
				}
			}
		})
	}
}
