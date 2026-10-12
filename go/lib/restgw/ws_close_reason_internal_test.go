package restgw

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWSCloseReason_FitsAndStaysValidUTF8(t *testing.T) {
	cases := []string{
		"",
		"short",
		strings.Repeat("a", wsMaxCloseReason),
		strings.Repeat("a", wsMaxCloseReason+1),
		strings.Repeat("ž", 100), // 2-byte runes
		strings.Repeat("中", 100), // 3-byte runes
		strings.Repeat("😀", 100), // 4-byte runes
		"a" + strings.Repeat("😀", 40),
	}
	for _, in := range cases {
		out := wsCloseReason(in)
		if len(out) > wsMaxCloseReason {
			t.Errorf("len %d → %d bytes, over %d", len(in), len(out), wsMaxCloseReason)
		}
		if !utf8.ValidString(out) {
			t.Errorf("len %d → invalid UTF-8 %q", len(in), out)
		}
		if len(in) <= wsMaxCloseReason {
			if out != in {
				t.Errorf("a reason that fits was changed: %q → %q", in, out)
			}
			continue
		}
		if !strings.HasPrefix(in, strings.TrimSuffix(out, "…")) || !strings.HasSuffix(out, "…") {
			t.Errorf("truncation is not a marked prefix: %q", out)
		}
	}
}
