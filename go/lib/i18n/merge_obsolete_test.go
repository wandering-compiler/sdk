package i18n_test

import (
	"strings"
	"testing"

	"github.com/leonelquinteros/gotext"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

const priorObsoleteCS = `msgid ""
msgstr ""
"Language: cs\n"

msgid "This field is required."
msgstr "Toto pole je povinné."

msgid "The value is too long."
msgstr "Hodnota je příliš dlouhá."

msgid "Untranslated sentence."
msgstr ""

#~ msgid "An old \"quoted\" sentence."
#~ msgstr "Stará \"citovaná\" věta."
`

func served(t *testing.T, body []byte) map[string]string {
	t.Helper()
	po := gotext.NewPo()
	po.Parse(body)
	out := map[string]string{}
	for _, tr := range po.GetDomain().GetTranslations() {
		if tr.ID != "" {
			out[tr.ID] = tr.Trs[0]
		}
	}
	return out
}

// A changed sentence keeps its translation as a fuzzy guess with the old
// msgid beside it; a sentence gone for good keeps its translation as an
// obsolete entry; an untranslated one leaves nothing behind.
func TestMergePO_ChangedSentenceInheritsAsFuzzy(t *testing.T) {
	out, err := i18n.MergePO([]byte(priorObsoleteCS), "cs", []string{"This field is required here."})
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	want := "#, fuzzy\n#| msgid \"This field is required.\"\nmsgid \"This field is required here.\"\nmsgstr \"Toto pole je povinné.\""
	if !strings.Contains(body, want) {
		t.Errorf("no fuzzy carry-over:\n%s", body)
	}
	if !strings.Contains(body, "#~ msgid \"The value is too long.\"\n#~ msgstr \"Hodnota je příliš dlouhá.\"") {
		t.Errorf("a translated entry no longer harvested was dropped:\n%s", body)
	}
	if !strings.Contains(body, `#~ msgid "An old \"quoted\" sentence."`) {
		t.Errorf("an obsolete entry was not carried:\n%s", body)
	}
	if strings.Contains(body, "Untranslated sentence.") {
		t.Errorf("an untranslated entry was kept as obsolete:\n%s", body)
	}
	// Nothing obsolete is served, and the fuzzy guess is not served either
	// (the runtime and EntriesFromPO skip it).
	s := served(t, out)
	if _, ok := s["The value is too long."]; ok {
		t.Error("gotext serves an obsolete entry")
	}
	for _, e := range i18n.EntriesFromPO(out) {
		if e.Msgid == "This field is required here." && (e.Msgstr != "" || !e.Fuzzy) {
			t.Errorf("fuzzy entry served: %+v", e)
		}
	}
	if err := i18n.ValidatePO(out); err == nil || !strings.Contains(err.Error(), "fuzzy") {
		t.Errorf("ValidatePO = %v; want the fuzzy warning only", err)
	}
}

// A msgid that comes back gets its translation back, not fuzzy; a merge of
// the merge changes nothing.
func TestMergePO_ComesBackAndIsStable(t *testing.T) {
	first, err := i18n.MergePO([]byte(priorObsoleteCS), "cs", []string{"This field is required."})
	if err != nil {
		t.Fatal(err)
	}
	back, err := i18n.MergePO(first, "cs", []string{"This field is required.", "The value is too long."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), "msgid \"The value is too long.\"\nmsgstr \"Hodnota je příliš dlouhá.\"") || strings.Contains(string(back), "#~ msgid \"The value is too long.\"") {
		t.Errorf("the translation did not come back:\n%s", back)
	}
	if strings.Contains(string(back), "#, fuzzy") {
		t.Errorf("a returning msgid came back fuzzy:\n%s", back)
	}
	again, err := i18n.MergePO(back, "cs", []string{"This field is required.", "The value is too long."})
	if err != nil {
		t.Fatal(err)
	}
	strip := func(b []byte) string {
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.Contains(l, "POT-Creation-Date") {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	if strip(again) != strip(back) {
		t.Errorf("merge is not stable:\n%s\n---\n%s", back, again)
	}
}

// In the default language the baseline copies the msgid: only an edited
// English sentence is worth keeping.
func TestMergePO_DefaultLanguageKeepsOnlyEditedCopy(t *testing.T) {
	prior := "msgid \"\"\nmsgstr \"\"\n\"Language: en\\n\"\n\nmsgid \"Plain.\"\nmsgstr \"Plain.\"\n\nmsgid \"Edited.\"\nmsgstr \"Edited by a copywriter.\"\n"
	out, err := i18n.MergePO([]byte(prior), "en", []string{"Fresh."})
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if strings.Contains(body, "#~ msgid \"Plain.\"") {
		t.Errorf("a baseline copy was kept as obsolete:\n%s", body)
	}
	if !strings.Contains(body, "#~ msgid \"Edited.\"") {
		t.Errorf("an edited sentence was dropped:\n%s", body)
	}
	if !strings.Contains(body, "msgid \"Fresh.\"\nmsgstr \"Fresh.\"") {
		t.Errorf("a new msgid lacks the baseline:\n%s", body)
	}
}

func TestStats(t *testing.T) {
	out, err := i18n.MergePO([]byte(priorObsoleteCS), "cs", []string{"This field is required here.", "Untranslated sentence.", "Brand new."})
	if err != nil {
		t.Fatal(err)
	}
	s := i18n.Stats(out)
	if s.Total != 3 || s.Fuzzy != 1 || s.Untranslated != 2 || s.Translated != 0 || s.Obsolete != 2 || len(s.Problems) != 0 {
		t.Errorf("Stats = %+v\n%s", s, out)
	}
}
