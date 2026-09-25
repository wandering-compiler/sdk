package i18n_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

const priorCS = `msgid ""
msgstr ""
"Language: cs\n"

msgid "hello"
msgstr ""

msgid "bye"
msgstr "nazdar"
`

// An UNTRANSLATED entry stays untranslated across a merge.
//
// It did not: MergePO read prior msgstrs through gotext's Translation.Get(),
// which returns the MSGID when the msgstr is empty. Every regen therefore wrote
// each untranslated entry back as "translated" to its own English source, so a
// catalog converged on fully-translated-in-English and a translator lost the one
// signal that says what is left to do. Silent, because the rendered text is the
// same either way — the only visible trace was two .po files that changed on
// every codegen, in a repo whose CI expects a clean tree.
func TestMergePO_LeavesAnUntranslatedEntryUntranslated(t *testing.T) {
	out, err := i18n.MergePO([]byte(priorCS), "cs", []string{"hello", "bye", "fresh"})
	if err != nil {
		t.Fatalf("MergePO: %v", err)
	}
	body := string(out)

	if strings.Contains(body, "msgid \"hello\"\nmsgstr \"hello\"") {
		t.Errorf("an untranslated entry came back translated to its own msgid:\n%s", body)
	}
	if !strings.Contains(body, "msgid \"hello\"\nmsgstr \"\"") {
		t.Errorf("the untranslated entry is not empty any more:\n%s", body)
	}
	// The two halves that must survive: a real translation, and a msgid the
	// prior catalog had never seen (which is why a merge runs at all).
	if !strings.Contains(body, "msgid \"bye\"\nmsgstr \"nazdar\"") {
		t.Errorf("a translator's work was lost:\n%s", body)
	}
	if !strings.Contains(body, "msgid \"fresh\"\nmsgstr \"\"") {
		t.Errorf("a newly harvested msgid did not arrive:\n%s", body)
	}
}

// EntriesFromPO is the other reader of the same field — the JSON catalog is
// baked from it — and it had the same fallback. Left unfixed, the .po would say
// untranslated and the FE's i18n.json would say translated, from one file.
func TestEntriesFromPO_ReportsAnUntranslatedEntryAsEmpty(t *testing.T) {
	for _, e := range i18n.EntriesFromPO([]byte(priorCS)) {
		if e.Msgid == "hello" && e.Msgstr != "" {
			t.Errorf("EntriesFromPO reports %q as translated to %q", e.Msgid, e.Msgstr)
		}
		if e.Msgid == "bye" && e.Msgstr != "nazdar" {
			t.Errorf("EntriesFromPO lost the translation for %q: %q", e.Msgid, e.Msgstr)
		}
	}
}
