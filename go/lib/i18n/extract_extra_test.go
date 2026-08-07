package i18n_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// EntriesFromMsgids mirrors the seed shape: EN gets msgstr=msgid,
// every other language gets an empty msgstr.
func TestEntriesFromMsgids_BaselineVsScaffold(t *testing.T) {
	en := i18n.EntriesFromMsgids("en", []string{"is required", "must be valid"})
	if len(en) != 2 {
		t.Fatalf("en entries = %d, want 2", len(en))
	}
	for _, e := range en {
		if e.Msgstr != e.Msgid {
			t.Errorf("en baseline: msgstr %q != msgid %q", e.Msgstr, e.Msgid)
		}
	}
	cs := i18n.EntriesFromMsgids("cs", []string{"is required"})
	if len(cs) != 1 || cs[0].Msgstr != "" {
		t.Errorf("cs scaffold: want single empty-msgstr entry, got %+v", cs)
	}
	if cs[0].Msgid != "is required" {
		t.Errorf("cs msgid = %q, want 'is required'", cs[0].Msgid)
	}
}

// EntriesFromMsgids on empty msgids returns an empty (non-nil) slice.
func TestEntriesFromMsgids_Empty(t *testing.T) {
	if got := i18n.EntriesFromMsgids("en", nil); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// EntriesFromPO round-trips a .po body back into POEntry records,
// preserving translator-authored msgstrs.
func TestEntriesFromPO_RoundTrip(t *testing.T) {
	body, err := i18n.MarshalPO("cs", []i18n.POEntry{
		{Msgid: "Active", Msgstr: "Aktivní"},
		{Msgid: "Pending", Msgstr: ""},
	})
	if err != nil {
		t.Fatalf("MarshalPO: %v", err)
	}
	entries := i18n.EntriesFromPO(body)
	byID := map[string]string{}
	for _, e := range entries {
		byID[e.Msgid] = e.Msgstr
	}
	if byID["Active"] != "Aktivní" {
		t.Errorf("Active msgstr = %q, want 'Aktivní'", byID["Active"])
	}
	if _, ok := byID["Pending"]; !ok {
		t.Errorf("Pending entry missing from round-trip: %v", byID)
	}
}

// EntriesFromPO on a malformed / empty body degrades to an empty
// result rather than panicking.
// TestEntriesFromPO_DuplicateMsgid pins the dedup contract: a hand-edited
// `.po` that carries the SAME msgid twice (a real hazard — translators copy
// blocks, merge conflicts duplicate stanzas) must collapse to exactly ONE
// entry, never two. The merge path keys catalogs by msgid; a duplicate that
// leaked through as two entries would double-count and could shadow a real
// translation with an empty one. We assert the count invariant (deterministic)
// and that the surviving msgstr is one of the two parsed values — without
// pinning gotext's first-vs-last choice, which is an implementation detail.
func TestEntriesFromPO_DuplicateMsgid(t *testing.T) {
	body := []byte(strings.Join([]string{
		`msgid ""`,
		`msgstr "Language: cs\n"`,
		``,
		`msgid "Active"`,
		`msgstr "Aktivni"`,
		``,
		`msgid "Active"`,
		`msgstr "Zapnuto"`,
		``,
	}, "\n"))

	entries := i18n.EntriesFromPO(body)

	var got []string
	for _, e := range entries {
		if e.Msgid == "Active" {
			got = append(got, e.Msgstr)
		}
	}
	if len(got) != 1 {
		t.Fatalf("duplicate msgid %q produced %d entries (%v), want exactly 1", "Active", len(got), got)
	}
	if got[0] != "Aktivni" && got[0] != "Zapnuto" {
		t.Errorf("surviving msgstr = %q, want one of the two parsed values", got[0])
	}
}

func TestEntriesFromPO_Malformed(t *testing.T) {
	if got := i18n.EntriesFromPO([]byte("this is not a real po file {{{")); len(got) != 0 {
		// gotext is lenient; assert no panic + no spurious entries with empty IDs.
		for _, e := range got {
			if e.Msgid == "" {
				t.Errorf("empty-msgid entry leaked: %+v", got)
			}
		}
	}
	if got := i18n.EntriesFromPO(nil); len(got) != 0 {
		t.Errorf("nil body: got %v, want empty", got)
	}
}

// MarshalPO carries a Reference into a `#:` line and skips a
// caller-supplied empty msgid (the header slot is package-owned).
func TestMarshalPO_ReferenceAndEmptyMsgidSkip(t *testing.T) {
	body, err := i18n.MarshalPO("en", []i18n.POEntry{
		{Msgid: "", Msgstr: "should be skipped"},
		{Msgid: "with ref", Msgstr: "x", Reference: "tasks/task.proto:42"},
	})
	if err != nil {
		t.Fatalf("MarshalPO: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "tasks/task.proto:42") {
		t.Errorf(".po missing reference line:\n%s", s)
	}
	if strings.Contains(s, "should be skipped") {
		t.Errorf("caller-supplied empty msgid clobbered the header:\n%s", s)
	}
}

// MarshalJSON rejects an empty lang and skips empty-msgid entries.
func TestMarshalJSON_EmptyLangAndEmptyMsgid(t *testing.T) {
	if _, err := i18n.MarshalJSON("", nil); err == nil {
		t.Error("MarshalJSON(\"\"): expected error, got nil")
	}
	body, err := i18n.MarshalJSON("en", []i18n.POEntry{
		{Msgid: "", Msgstr: "skip"},
		{Msgid: "keep", Msgstr: "kept"},
	})
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"keep"`) || !strings.Contains(s, `"kept"`) {
		t.Errorf("JSON missing kept entry:\n%s", s)
	}
	if strings.Contains(s, `"skip"`) {
		t.Errorf("empty-msgid entry leaked into JSON:\n%s", s)
	}
}

// MarshalJSON escapes unicode + control-bearing msgids/msgstrs
// safely (json.Marshal path) and stays parseable.
func TestMarshalJSON_UnicodeAndQuotes(t *testing.T) {
	body, err := i18n.MarshalJSON("cs", []i18n.POEntry{
		{Msgid: `quote "x" tab`, Msgstr: "řádek\nnový"},
		{Msgid: "emoji 🚀", Msgstr: "ok"},
	})
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	s := string(body)
	// Embedded quotes must be escaped, newline encoded as \n.
	if !strings.Contains(s, `\"x\"`) {
		t.Errorf("embedded quotes not escaped:\n%s", s)
	}
	if !strings.Contains(s, `\n`) {
		t.Errorf("newline not encoded:\n%s", s)
	}
}

// MergePO rejects an empty lang, and a new msgid (absent from the
// prior catalog) takes the EN baseline when lang is the compile-time
// default.
func TestMergePO_EmptyLangAndNewBaseline(t *testing.T) {
	if _, err := i18n.MergePO(nil, "", []string{"x"}); err == nil {
		t.Error("MergePO(\"\"): expected error, got nil")
	}
	prior, err := i18n.MarshalPO("en", []i18n.POEntry{
		{Msgid: "Active", Msgstr: "Active"},
	})
	if err != nil {
		t.Fatalf("setup MarshalPO: %v", err)
	}
	merged, err := i18n.MergePO(prior, "en", []string{"Active", "Brand New"})
	if err != nil {
		t.Fatalf("MergePO: %v", err)
	}
	// EN new msgid gets msgstr == msgid baseline.
	if !strings.Contains(string(merged), `msgid "Brand New"`+"\n"+`msgstr "Brand New"`) {
		t.Errorf("EN new msgid did not take baseline msgstr:\n%s", merged)
	}
}

// EmitDomainCatalogs surfaces the underlying MarshalPO error when a
// language tag is empty (an empty tag routes to ScaffoldPO → MarshalPO,
// which refuses an empty Language header).
func TestEmitDomainCatalogs_PropagatesEmptyLangError(t *testing.T) {
	_, err := i18n.EmitDomainCatalogs("app", "w17/languages",
		[]string{""}, []string{"is required"})
	if err == nil {
		t.Error("expected error from empty language tag, got nil")
	}
}
