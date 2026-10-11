package i18n_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

func inLang(lang string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, lang))
}

// A message the requested catalog lacks falls back to the default-language
// catalog before the msgid — per message, not per catalog. A partially
// translated catalog must not render its gaps in source text while the
// default language has them.
func TestT_FallbackIsPerMessage(t *testing.T) {
	resetState(t)
	t.Cleanup(i18n.Reset)
	i18n.SetDefaultLanguage("de")
	i18n.Register("de", []byte(minimalPO("de", "Bye", "Tschuess")))
	i18n.Register("cs", []byte(minimalPO("cs", "Hello", "Ahoj")))

	ctx := inLang("cs")
	if got := i18n.T(ctx, "Hello", nil); got != "Ahoj" {
		t.Errorf("T(cs, Hello) = %q, want Ahoj (requested catalog)", got)
	}
	if got := i18n.T(ctx, "Bye", nil); got != "Tschuess" {
		t.Errorf("T(cs, Bye) = %q, want Tschuess (default-language catalog, cs lacks it)", got)
	}
	if got := i18n.T(ctx, "Unknown", nil); got != "Unknown" {
		t.Errorf("T(cs, Unknown) = %q, want the msgid", got)
	}
}

// An entry present with an empty msgstr is "untranslated" in gettext and
// must fall through exactly like an absent one.
func TestT_EmptyMsgstrFallsThrough(t *testing.T) {
	resetState(t)
	t.Cleanup(i18n.Reset)
	i18n.SetDefaultLanguage("de")
	i18n.Register("de", []byte(minimalPO("de", "Bye", "Tschuess")))
	i18n.Register("cs", []byte(minimalPO("cs", "Bye", "")))
	if got := i18n.T(inLang("cs"), "Bye", nil); got != "Tschuess" {
		t.Errorf("T(cs, Bye) = %q, want Tschuess", got)
	}
}

// Substitution is single-pass: a parameter value that contains another
// placeholder token stays literal, and the result is the same on every
// call (it must not depend on map iteration order).
func TestSubstitute_DeterministicSinglePass(t *testing.T) {
	resetState(t)
	params := map[string]string{"name": "{max}", "max": "5", "a": "{b}", "b": "{a}"}
	const want = "hello {max}, limit 5, {b}{a}"
	for i := 0; i < 300; i++ {
		got := i18n.T(context.Background(), "hello {name}, limit {max}, {a}{b}", params)
		if got != want {
			t.Fatalf("run %d: T = %q, want %q", i, got, want)
		}
	}
}

// Brace edge cases: an unclosed brace, a brace inside a name and a doubled
// brace around a known name all pass through without eating text.
func TestSubstitute_BraceEdges(t *testing.T) {
	resetState(t)
	p := map[string]string{"max": "5"}
	cases := map[string]string{
		"{{max}}":     "{5}",
		"a {max":      "a {max",
		"{x{max}}":    "{x5}",
		"}{max}{":     "}5{",
		"{}":          "{}",
		"{max}{max}":  "55",
		"no braces":   "no braces",
		"{unknown} {": "{unknown} {",
	}
	for in, want := range cases {
		if got := i18n.T(context.Background(), in, p); got != want {
			t.Errorf("T(%q) = %q, want %q", in, got, want)
		}
	}
}

// Catalog tags match regardless of `_`/`-` spelling and case: a gettext
// `pt_BR.po` serves a BCP-47 `pt-BR` request, directly and through
// Accept-Language negotiation.
func TestCatalogTag_UnderscoreAndCaseInsensitive(t *testing.T) {
	resetState(t)
	t.Cleanup(i18n.Reset)
	mfs := fstest.MapFS{
		"i18n/pt_BR.po": {Data: []byte(minimalPO("pt_BR", "hello", "olá"))},
		"i18n/en.po":    {Data: []byte(minimalPO("en", "hello", "hi"))},
	}
	if _, err := i18n.RegisterFS(mfs); err != nil {
		t.Fatalf("RegisterFS: %v", err)
	}
	for _, req := range []string{"pt-BR", "pt_BR", "PT-br", "de-DE,pt-BR;q=0.9"} {
		if got := i18n.T(inLang(req), "hello", nil); got != "olá" {
			t.Errorf("T(%q) = %q, want olá (pt_BR.po)", req, got)
		}
	}
	// The default language is normalised the same way.
	i18n.SetDefaultLanguage("PT_br")
	if got := i18n.T(inLang("fr"), "hello", nil); got != "olá" {
		t.Errorf("T(fr) with default PT_br = %q, want olá", got)
	}
}

// Two registrations for one language merge: each keeps the messages only
// it translates; a conflicting translation keeps the first and is
// reported.
func TestRegisterCatalog_MergesAndReportsConflicts(t *testing.T) {
	resetState(t)
	t.Cleanup(i18n.Reset)
	if err := i18n.RegisterCatalog("cs", []byte(minimalPO("cs", "a", "A1"))); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := i18n.RegisterCatalog("cs", []byte(minimalPO("cs", "b", "B1"))); err != nil {
		t.Fatalf("disjoint second: %v", err)
	}
	if err := i18n.RegisterCatalog("cs", []byte(minimalPO("cs", "a", "A1"))); err != nil {
		t.Fatalf("identical re-registration must not be a conflict: %v", err)
	}
	err := i18n.RegisterCatalog("cs", []byte(minimalPO("cs", "a", "A2")))
	if err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("conflicting translation: err = %v, want a conflict naming msgid a", err)
	}
	ctx := inLang("cs")
	if got := i18n.T(ctx, "a", nil); got != "A1" {
		t.Errorf("T(a) = %q, want A1 (first registration kept)", got)
	}
	if got := i18n.T(ctx, "b", nil); got != "B1" {
		t.Errorf("T(b) = %q, want B1 (merged from second registration)", got)
	}
}

func TestValidatePO(t *testing.T) {
	good := minimalPO("cs", "a", "b") + `
msgctxt "menu"
msgid "a"
msgstr "c"

msgid "one"
msgid_plural "many"
msgstr[0] "jeden"
msgstr[1] "více"

# comment
#~ msgid "obsolete"
msgid ""
"multi"
"line"
msgstr ""
"víc"
"řádků"
`
	if err := i18n.ValidatePO([]byte(good)); err != nil {
		t.Errorf("ValidatePO(good) = %v", err)
	}
	if err := i18n.ValidatePO(nil); err != nil {
		t.Errorf("ValidatePO(empty) = %v", err)
	}
	body, err := i18n.MarshalPO("cs", []i18n.POEntry{{Msgid: "x \"q\"\nnl", Msgstr: "y"}, {Msgid: "z"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := i18n.ValidatePO(body); err != nil {
		t.Errorf("ValidatePO(MarshalPO output) = %v\n%s", err, body)
	}

	bad := map[string]string{
		"missing msgstr":   "msgid \"a\"\n",
		"unterminated":     "msgid \"a\nmsgstr \"b\"\n",
		"stray text":       "msgid \"a\"\nmsgstr \"b\"\ngarbage\n",
		"msgstr first":     "msgstr \"b\"\n",
		"duplicate msgid":  "msgid \"a\"\nmsgstr \"b\"\nmsgid \"a\"\nmsgstr \"c\"\n",
		"plural unindexed": "msgid \"a\"\nmsgid_plural \"as\"\nmsgstr \"b\"\n",
		"index no plural":  "msgid \"a\"\nmsgstr[0] \"b\"\n",
		"double msgid":     "msgid \"a\"\nmsgid \"b\"\nmsgstr \"c\"\n",
		"orphan string":    "\"x\"\n",
	}
	for name, body := range bad {
		if err := i18n.ValidatePO([]byte(body)); err == nil {
			t.Errorf("%s: ValidatePO = nil, want an error", name)
		} else if !strings.Contains(err.Error(), "line ") {
			t.Errorf("%s: error %q does not name a line", name, err)
		}
	}
}

// A malformed catalog is reported by RegisterFS (named by path) while the
// good ones — and whatever parses of the bad one — still serve.
func TestRegisterFS_ReportsMalformedCatalog(t *testing.T) {
	resetState(t)
	t.Cleanup(i18n.Reset)
	broken := minimalPO("de", "hello", "hallo") + "msgid \"cut off\nmsgstr\n"
	mfs := fstest.MapFS{
		"i18n/cs.po": {Data: []byte(minimalPO("cs", "hello", "ahoj"))},
		"i18n/de.po": {Data: []byte(broken)},
	}
	langs, err := i18n.RegisterFS(mfs)
	if err == nil || !strings.Contains(err.Error(), "i18n/de.po") {
		t.Fatalf("RegisterFS err = %v, want a report naming i18n/de.po", err)
	}
	if strings.Contains(err.Error(), "cs.po") {
		t.Errorf("RegisterFS blamed the good catalog: %v", err)
	}
	if len(langs) != 2 {
		t.Errorf("langs = %v, want both registered", langs)
	}
	if got := i18n.T(inLang("cs"), "hello", nil); got != "ahoj" {
		t.Errorf("T(cs) = %q, want ahoj", got)
	}
	if got := i18n.T(inLang("de"), "hello", nil); got != "hallo" {
		t.Errorf("T(de) = %q, want hallo (best-effort registration)", got)
	}
}

// A tree without a single catalog is reported, not silently accepted.
func TestRegisterFS_NoCatalogIsReported(t *testing.T) {
	resetState(t)
	_, err := i18n.RegisterFS(fstest.MapFS{"i18n/README.md": {Data: []byte("x")}})
	if !errors.Is(err, i18n.ErrNoCatalogs) {
		t.Errorf("RegisterFS(no .po) err = %v, want ErrNoCatalogs", err)
	}
}

// An explicit format locale on OUTGOING metadata (the gateway tier, which
// has no incoming metadata) wins over the UI language, as it does on
// incoming metadata.
func TestFormatLocaleFromContext_OutgoingMetadata(t *testing.T) {
	resetState(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		i18n.LanguageMetadataKey, "en",
		i18n.FormatLocaleMetadataKey, "cs")
	if got := i18n.FormatLocaleFromContext(ctx); got != "cs" {
		t.Errorf("FormatLocaleFromContext(outgoing cs, language en) = %q, want cs", got)
	}
	// Incoming outranks outgoing, mirroring LocaleFromContext.
	in := metadata.NewIncomingContext(ctx, metadata.Pairs(i18n.FormatLocaleMetadataKey, "de"))
	if got := i18n.FormatLocaleFromContext(in); got != "de" {
		t.Errorf("FormatLocaleFromContext(incoming de, outgoing cs) = %q, want de", got)
	}
}
