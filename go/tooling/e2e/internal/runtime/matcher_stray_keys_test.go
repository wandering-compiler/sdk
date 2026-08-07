package runtime

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A matcher spec carrying a key the matcher has no slot for must be
// REFUSED, not ignored.
//
// The failure this closes is invisible by construction. In a YAML flow
// mapping the comma separates entries, so
//
//	headline: {matcher: eq, value: Backend engineer, Prague}
//
// is THREE entries: `matcher`, `value: Backend engineer`, and a key
// `Prague` with no value. The decoder used to read `matcher` and `value`
// and drop the rest, so the case ran, passed, and asserted half the
// string its author wrote — with nothing anywhere reporting a problem.
// Found by an unrelated break-proof run happening to compare the two
// halves; nothing in the harness could have named it.
func TestDecodeMatcher_RefusesAStrayKeyFromAnUnquotedComma(t *testing.T) {
	// Parsed from YAML rather than hand-built, because the shape under
	// test is a property of YAML's flow syntax and a hand-built map
	// would be asserting the author's intent instead of the parser's.
	var spec any
	const src = `{matcher: eq, value: Backend engineer, Prague}`
	if err := yaml.Unmarshal([]byte(src), &spec); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	m, ok := asStringMap(spec)
	if !ok {
		t.Fatalf("%s did not parse as a mapping", src)
	}
	if len(m) != 3 {
		t.Fatalf("%s parsed to %d entries (%v); this test is about the flow-comma splitting one value into two entries, so if YAML stopped doing that there is nothing here to guard", src, len(m), m)
	}

	_, err := DecodeMatcher(spec)
	if err == nil {
		t.Fatal("DecodeMatcher accepted a spec with a stray key. The value was silently truncated at the comma, so the case would run and pass against half the expected string.")
	}
	// The message has to name the cause, not just the symptom: an author
	// looking at `{matcher: eq, value: Backend engineer, Prague}` does not
	// see a stray key, they see one sentence.
	for _, want := range []string{"Prague", "QUOTE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q; got: %v", want, err)
		}
	}
}

// The well-formed specs must all still decode — a strictness change that
// rejects valid authoring would be worse than the bug it fixes.
func TestDecodeMatcher_AcceptsEveryWellFormedSpec(t *testing.T) {
	for _, src := range []string{
		`{matcher: not_empty}`,
		`{matcher: empty}`,
		`{matcher: eq, value: "Backend engineer, Prague"}`,
		`{matcher: eq, value: 4}`,
		`{matcher: regex, pattern: "^abc"}`,
		`{matcher: count, op: '>=', value: 2}`,
		`{matcher: num, op: '==', value: 1}`,
		`{matcher: distinct, field: id, op: '==', value: 3}`,
		`{capture: card.id}`,
		`{capture: card.id, match: {matcher: not_empty}}`,
		// A mapping with NO `matcher` stays a nested partial match, so
		// arbitrary keys are legal there and must not be swept up.
		`{account: {id: {capture: x}}}`,
	} {
		var spec any
		if err := yaml.Unmarshal([]byte(src), &spec); err != nil {
			t.Fatalf("yaml %s: %v", src, err)
		}
		if _, err := DecodeMatcher(spec); err != nil {
			t.Errorf("DecodeMatcher(%s) = %v, want no error", src, err)
		}
	}
}

// An unrecognised KIND keeps reporting the kind. Reporting its keys first
// would answer a question the author did not ask — they misspelled the
// matcher, and every key looks stray once the kind is unknown.
func TestDecodeMatcher_UnknownKindStillReportsTheKind(t *testing.T) {
	_, err := DecodeMatcher(map[string]any{"matcher": "eqq", "value": "x"})
	if err == nil {
		t.Fatal("an unknown matcher kind was accepted")
	}
	if !strings.Contains(err.Error(), "eqq") {
		t.Errorf("refusal does not name the unknown kind: %v", err)
	}
}
