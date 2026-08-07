package i18n_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// TestMarshalPO_BasicShape — output contains the gettext
// header block + each entry as msgid/msgstr pairs, sorted
// by msgid.
func TestMarshalPO_BasicShape(t *testing.T) {
	body, err := i18n.MarshalPO("cs", []i18n.POEntry{
		{Msgid: "is required", Msgstr: "je požadováno"},
		{Msgid: "may not be blank", Msgstr: ""},
	})
	if err != nil {
		t.Fatalf("MarshalPO: %v", err)
	}
	s := string(body)
	wantSubs := []string{
		`msgid ""`, // header msgid (empty)
		`"Language: cs\n"`,
		`"MIME-Version: 1.0\n"`,
		`"Content-Type: text/plain; charset=UTF-8\n"`,
		`msgid "is required"`,
		`msgstr "je požadováno"`,
		`msgid "may not be blank"`,
		// scaffolded entry has empty msgstr
		`msgstr ""`,
	}
	for _, sub := range wantSubs {
		if !strings.Contains(s, sub) {
			t.Errorf(".po missing %q\n--- .po ---\n%s", sub, s)
		}
	}

	// Entries sorted: "is required" should appear before
	// "may not be blank" in the output.
	if i, j := strings.Index(s, `msgid "is required"`),
		strings.Index(s, `msgid "may not be blank"`); i < 0 || j < 0 || i > j {
		t.Errorf("entries not sorted: 'is required' at %d, 'may not be blank' at %d", i, j)
	}
}

// TestMarshalPO_EmptyLang_Errors — gettext consumers reject
// missing Language: header; refuse the call rather than emit
// a half-broken file.
func TestMarshalPO_EmptyLang_Errors(t *testing.T) {
	_, err := i18n.MarshalPO("", nil)
	if err == nil {
		t.Error("MarshalPO(\"\"): expected error, got nil")
	}
}

// TestScaffoldPO_AllMsgstrEmpty — scaffolded file has every
// msgstr empty (translator fills in).
func TestScaffoldPO_AllMsgstrEmpty(t *testing.T) {
	body, err := i18n.ScaffoldPO("cs", []string{"is required", "must be valid"})
	if err != nil {
		t.Fatalf("ScaffoldPO: %v", err)
	}
	s := string(body)
	// Both entries present + their msgstr lines are empty.
	for _, msgid := range []string{"is required", "must be valid"} {
		idx := strings.Index(s, `msgid "`+msgid+`"`)
		if idx < 0 {
			t.Errorf("msgid %q missing from scaffolded body:\n%s", msgid, s)
			continue
		}
		// Next msgstr line after this msgid should be the
		// empty form.
		rest := s[idx:]
		msgstrIdx := strings.Index(rest, "msgstr ")
		if msgstrIdx < 0 {
			t.Errorf("no msgstr after msgid %q", msgid)
			continue
		}
		line := rest[msgstrIdx : msgstrIdx+len(`msgstr ""`)]
		if line != `msgstr ""` {
			t.Errorf("msgid %q: msgstr line = %q, want %q", msgid, line, `msgstr ""`)
		}
	}
}

// TestEmitDomainCatalogs_PerLanguageOutput — for two
// languages [en, cs] the helper produces two project-relative
// File entries, en uses baseline (msgstr=msgid), cs is
// scaffolded (empty msgstr). Path layout is
// `<languagesDir>/<domain>/<lang>.po` flat (no inner
// `languages/` subdir, REV-149 P1.8).
func TestEmitDomainCatalogs_PerLanguageOutput(t *testing.T) {
	files, err := i18n.EmitDomainCatalogs("app", "w17/languages",
		[]string{"en", "cs"},
		[]string{"is required", "must be valid"})
	if err != nil {
		t.Fatalf("EmitDomainCatalogs: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len = %d, want 2", len(files))
	}
	byLang := map[string]i18n.DomainCatalogFile{}
	for _, f := range files {
		byLang[f.Lang] = f
	}
	enF, ok := byLang["en"]
	if !ok {
		t.Fatal("en entry missing")
	}
	if enF.Path != "w17/languages/app/en.po" {
		t.Errorf("en path = %q, want w17/languages/app/en.po", enF.Path)
	}
	if !strings.Contains(string(enF.Body), `msgid "is required"`) ||
		!strings.Contains(string(enF.Body), `msgstr "is required"`) {
		t.Errorf("en body missing baseline 'is required' entry:\n%s", enF.Body)
	}
	csF, ok := byLang["cs"]
	if !ok {
		t.Fatal("cs entry missing")
	}
	if csF.Path != "w17/languages/app/cs.po" {
		t.Errorf("cs path = %q, want w17/languages/app/cs.po", csF.Path)
	}
	// cs is scaffolded — msgid present, msgstr empty for "is required"
	idx := strings.Index(string(csF.Body), `msgid "is required"`)
	if idx < 0 {
		t.Fatalf("cs body missing 'is required' msgid")
	}
	window := string(csF.Body)[idx:]
	if len(window) > 200 {
		window = window[:200]
	}
	if !strings.Contains(window, `msgstr ""`) {
		t.Errorf("cs body 'is required' msgstr not empty:\n%s", window)
	}
}

// TestEmitDomainCatalogs_EmptyLanguagesDefaultsToEN — passing
// nil / empty languages collapses to ["en"] (the baseline-only
// catalog every project carries).
func TestEmitDomainCatalogs_EmptyLanguagesDefaultsToEN(t *testing.T) {
	files, err := i18n.EmitDomainCatalogs("app", "w17/languages", nil,
		[]string{"is required"})
	if err != nil {
		t.Fatalf("EmitDomainCatalogs: %v", err)
	}
	if len(files) != 1 || files[0].Lang != "en" {
		t.Errorf("expected single en entry, got %+v", files)
	}
}

// TestEmitDomainCatalogs_RejectsEmptyDomainOrDir — emit
// refuses missing identity rather than silently writing to
// "//en.po".
func TestEmitDomainCatalogs_RejectsEmptyDomainOrDir(t *testing.T) {
	if _, err := i18n.EmitDomainCatalogs("", "w17/languages",
		[]string{"en"}, []string{"x"}); err == nil {
		t.Error("expected error on empty domain")
	}
	if _, err := i18n.EmitDomainCatalogs("app", "",
		[]string{"en"}, []string{"x"}); err == nil {
		t.Error("expected error on empty languagesDir")
	}
}

// TestDefaultLocalePO_MsgstrMirrorsMsgid — EN baseline file
// has each msgstr == msgid.
func TestDefaultLocalePO_MsgstrMirrorsMsgid(t *testing.T) {
	body, err := i18n.DefaultLocalePO("en", []string{"is required"})
	if err != nil {
		t.Fatalf("DefaultLocalePO: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `msgid "is required"`) || !strings.Contains(s, `msgstr "is required"`) {
		t.Errorf("baseline EN entry malformed:\n%s", s)
	}
}

// TestMarshalPO_RoundTripsThroughRegister — emit a body, feed
// it back through Register, T() returns the encoded msgstr.
// Confirms end-to-end the extractor and the runtime speak the
// same dialect.
func TestMarshalPO_RoundTripsThroughRegister(t *testing.T) {
	i18n.Reset()
	body, err := i18n.MarshalPO("cs", []i18n.POEntry{
		{Msgid: "must be at most {max} characters", Msgstr: "musí být nejvýše {max} znaků"},
	})
	if err != nil {
		t.Fatalf("MarshalPO: %v", err)
	}
	i18n.Register("cs", body)

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, "cs"))
	got := i18n.T(ctx, "must be at most {max} characters", map[string]string{"max": "10"})
	want := "musí být nejvýše 10 znaků"
	if got != want {
		t.Errorf("round-trip T = %q, want %q", got, want)
	}
}
