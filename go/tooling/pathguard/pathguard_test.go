package pathguard

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestJoin(t *testing.T) {
	base := filepath.FromSlash("/tmp/stage")
	cases := []struct {
		rel    string
		ok     bool
		wantTo string // expected dst (slash form) when ok
	}{
		// contained — accepted
		{"a.proto", true, "/tmp/stage/a.proto"},
		{"domains/x/y.proto", true, "/tmp/stage/domains/x/y.proto"},
		{"a/../b.proto", true, "/tmp/stage/b.proto"}, // normalises but stays in
		{"./a.proto", true, "/tmp/stage/a.proto"},
		// forward-slash input normalised to host sep
		{"domains/x/w17.proto", true, "/tmp/stage/domains/x/w17.proto"},

		// escapes — rejected
		{"", false, ""},
		{"/etc/passwd", false, ""},        // absolute
		{"..", false, ""},                 // the parent
		{"../escape", false, ""},          // climb out
		{"../../etc/cron.d/x", false, ""}, // deep climb
		{"a/../../escape", false, ""},     // climb after descend
		{".", false, ""},                  // the base itself (RemoveAll-the-root)
		{"a/../..", false, ""},            // resolves to parent
	}
	for _, c := range cases {
		got, err := Join(base, c.rel)
		if c.ok {
			if err != nil {
				t.Errorf("Join(%q) unexpected error: %v", c.rel, err)
				continue
			}
			want := filepath.FromSlash(c.wantTo)
			if got != want {
				t.Errorf("Join(%q) = %q, want %q", c.rel, got, want)
			}
		} else if err == nil {
			t.Errorf("Join(%q) = %q, want rejection", c.rel, got)
		}
	}
}

func TestContains(t *testing.T) {
	base := filepath.FromSlash("/proj")
	ok := []string{"/proj", "/proj/a", "/proj/a/b/c"} // base itself allowed (unlike Join)
	bad := []string{"/etc", "/proj/../etc", "/", "/projfoo"}
	for _, p := range ok {
		if err := Contains(base, filepath.FromSlash(p)); err != nil {
			t.Errorf("Contains(%q) unexpected error: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := Contains(base, filepath.FromSlash(p)); err == nil {
			t.Errorf("Contains(%q) want rejection", p)
		}
	}
}

func TestControlCharsRejected(t *testing.T) {
	// A filename with an embedded newline (or other control char) is malformed /
	// an injection attempt (it would break out of an unquoted YAML scalar in a
	// generated buf.gen.yaml). Both entry guards must reject it.
	for _, bad := range []string{"x.proto\nvalue: pwn", "a\tb", "a\rb", "a\x00b"} {
		if _, err := Join(filepath.FromSlash("/b"), bad); err == nil {
			t.Errorf("Join(%q) should reject a control char", bad)
		}
		if err := ValidateRel(bad); err == nil {
			t.Errorf("ValidateRel(%q) should reject a control char", bad)
		}
	}
}

func TestValidateRel(t *testing.T) {
	ok := []string{".", "w17/services", "a/b/c", "./out"} // `.` permitted (output dir = root)
	bad := []string{"/abs", "..", "../x", "../../etc"}
	for _, p := range ok {
		if err := ValidateRel(p); err != nil {
			t.Errorf("ValidateRel(%q) unexpected error: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := ValidateRel(p); err == nil {
			t.Errorf("ValidateRel(%q) want rejection", p)
		}
	}
}

// TestJoinMatchesLegacy pins Join to the exact accept/reject set the two
// hand-copied guards (safeStageJoin, containedJoin) enforced, so the unify
// can't silently widen or narrow the security boundary.
func TestJoinMatchesLegacy(t *testing.T) {
	base := filepath.FromSlash("/b")
	legacy := func(base, rel string) bool { // returns true == accepted
		clean := filepath.FromSlash(rel)
		if clean == "" || filepath.IsAbs(clean) {
			return false
		}
		dst := filepath.Join(base, clean)
		r, err := filepath.Rel(base, dst)
		if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return false
		}
		return true
	}
	for _, rel := range []string{
		"a", "a/b", "a/../b", "./a", "", "/x", "..", "../x", "../../x",
		"a/../..", ".", "a/./b", "a/b/../../c", "a/b/../../../x",
	} {
		want := legacy(base, rel)
		_, err := Join(base, rel)
		got := err == nil
		if got != want {
			t.Errorf("Join(%q): got accepted=%v, legacy accepted=%v", rel, got, want)
		}
	}
}
