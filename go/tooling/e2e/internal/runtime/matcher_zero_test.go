package runtime

import (
	"strings"
	"testing"
)

// `{matcher: X, capture: y}` checks AND binds, on one line.
//
// It used to be refused: `capture` claimed the whole mapping, so `matcher`
// came back as a stray key and the message named the two keys capture
// accepts. That reads as "capture cannot check", and deinvo concluded the
// two had to be split (2026-09-05) — which puts the assertion somewhere
// other than where it belongs, or drops it.
func TestDecodeMatcher_CaptureCombinesWithAnyMatcher(t *testing.T) {
	m, err := DecodeMatcher(map[string]any{"matcher": "not_empty", "capture": "doc_id"})
	if err != nil {
		t.Fatalf("capture beside a matcher was refused: %v", err)
	}
	scope := NewRun().NewScope()
	if err := m.Match("abc123", true, scope); err != nil {
		t.Fatalf("a non-empty value failed the combined matcher: %v", err)
	}
	if got, ok := scope.Get("doc_id"); !ok || got != "abc123" {
		t.Errorf("capture bound %v (found=%v), want abc123 — the value was checked but never bound", got, ok)
	}
	// The CHECK half must still bite: an empty value fails, and nothing
	// is bound from a value the matcher rejected.
	if err := m.Match("", true, NewRun().NewScope()); err == nil {
		t.Error("an empty value passed `not_empty` when a capture rode along — the check was dropped")
	}
}

// The older spelling keeps working; it is the one the docs show.
func TestDecodeMatcher_CaptureWithNestedMatchStillWorks(t *testing.T) {
	m, err := DecodeMatcher(map[string]any{
		"capture": "x",
		"match":   map[string]any{"matcher": "not_empty"},
	})
	if err != nil {
		t.Fatalf("capture + match: %v", err)
	}
	if err := m.Match("v", true, NewRun().NewScope()); err != nil {
		t.Fatalf("capture + match rejected a good value: %v", err)
	}
}

// Naming the same slot twice is an authoring mistake, not a shape to
// interpret.
func TestDecodeMatcher_CaptureRefusesBothSpellingsAtOnce(t *testing.T) {
	_, err := DecodeMatcher(map[string]any{
		"capture": "x",
		"matcher": "not_empty",
		"match":   map[string]any{"matcher": "empty"},
	})
	if err == nil {
		t.Fatal("capture with BOTH `match` and `matcher` was accepted; one of the two is silently ignored")
	}
}

// A zero assertion cannot pass, so the message has to name the fix.
//
// proto3 JSON omits defaults: a field holding 0 is not on the wire, and
// "zero" arrives identically to "missing". The author is not wrong and no
// rewording of the assertion helps — only presence does.
func TestExactMatcher_ZeroAgainstAnAbsentFieldExplainsPresence(t *testing.T) {
	for _, zero := range []any{0, 0.0, "", false} {
		m, err := DecodeMatcher(zero)
		if err != nil {
			t.Fatalf("DecodeMatcher(%v): %v", zero, err)
		}
		err = m.Match(nil, false, NewRun().NewScope())
		if err == nil {
			t.Fatalf("expected %v against an absent field passed", zero)
		}
		for _, want := range []string{"proto3 omits default", "optional"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("expected %v: message %q does not mention %q", zero, err, want)
			}
		}
	}
}

// A NON-default expectation keeps the short message: the long one is for
// the case that cannot pass however it is written, and spending it on an
// ordinary mismatch would bury that.
func TestExactMatcher_NonZeroAbsenceKeepsTheShortMessage(t *testing.T) {
	m, _ := DecodeMatcher(7)
	err := m.Match(nil, false, NewRun().NewScope())
	if err == nil {
		t.Fatal("expected 7 against an absent field passed")
	}
	if strings.Contains(err.Error(), "proto3") {
		t.Errorf("a non-zero expectation got the presence lecture: %q", err)
	}
}
