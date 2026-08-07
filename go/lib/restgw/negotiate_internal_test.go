package restgw

import "testing"

// TestMediaSpecificity pins the Accept-tiebreak ranking: `*/*` (0) <
// `type/*` (1) < fully specified `type/subtype` (2).
func TestMediaSpecificity(t *testing.T) {
	cases := map[string]int{
		"*/*":              0,
		"application/*":    1,
		"text/*":           1,
		"application/json": 2,
		MIMEProtobuf:       2,
	}
	for mt, want := range cases {
		if got := mediaSpecificity(mt); got != want {
			t.Errorf("mediaSpecificity(%q) = %d, want %d", mt, got, want)
		}
	}
}

// TestParseAcceptEntry pins the per-entry parse: bare media type lower-
// cased, q weight (default 1.0 when absent or malformed), non-q params
// ignored, and ok=false for an empty media type.
func TestParseAcceptEntry(t *testing.T) {
	cases := []struct {
		entry  string
		wantMT string
		wantQ  float64
		wantOK bool
	}{
		{"application/json", "application/json", 1.0, true},
		{"TEXT/HTML;q=0.8", "text/html", 0.8, true},
		// malformed q value → keep the 1.0 default
		{"text/html;q=notafloat", "text/html", 1.0, true},
		// non-q parameter with no '=' is ignored (charset bareword)
		{"text/html;charset;q=0.5", "text/html", 0.5, true},
		// non-q parameter with '=' is ignored
		{"text/html;charset=utf-8", "text/html", 1.0, true},
		// empty media type → not ok
		{";q=0.5", "", 0, false},
		{"", "", 0, false},
	}
	for _, c := range cases {
		mt, q, ok := parseAcceptEntry(c.entry)
		if mt != c.wantMT || q != c.wantQ || ok != c.wantOK {
			t.Errorf("parseAcceptEntry(%q) = (%q,%v,%v), want (%q,%v,%v)",
				c.entry, mt, q, ok, c.wantMT, c.wantQ, c.wantOK)
		}
	}
}
