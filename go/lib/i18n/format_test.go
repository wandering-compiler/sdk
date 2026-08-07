package i18n

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
)

// formatVector is one row of testdata/format_vectors.json — the file the TS
// admin runtime runs too. Either `value` (the decimal-string / temporal path)
// or `float` (the double adapter) is set, never both.
type formatVector struct {
	Name   string   `json:"name"`
	Locale string   `json:"locale"`
	Kind   string   `json:"kind"`
	Places *int     `json:"places"`
	Value  *string  `json:"value"`
	Float  *float64 `json:"float"`
	Want   string   `json:"want"`
}

func loadFormatVectors(t *testing.T) []formatVector {
	t.Helper()
	body, err := os.ReadFile("testdata/format_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var doc struct {
		Schema int            `json:"schema"`
		Cases  []formatVector `json:"cases"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("vectors file carries no cases")
	}
	return doc.Cases
}

// applyVector runs one vector through the public formatting API. The TS mirror
// has the same switch; keeping the two dispatches identical is the point.
func applyVector(v formatVector) string {
	f := ResolveFormats(v.Locale)
	raw := ""
	switch {
	case v.Float != nil:
		raw = DecimalString(*v.Float)
	case v.Value != nil:
		raw = *v.Value
	}
	places := -1
	if v.Places != nil {
		places = *v.Places
	}
	switch v.Kind {
	case "number":
		return FormatNumber(raw, f)
	case "decimal":
		return FormatDecimal(raw, places, f)
	case "percent":
		return FormatPercent(raw, places, f)
	case "date":
		return FormatDate(raw, f)
	case "short_date":
		return FormatShortDate(raw, f)
	case "time":
		return FormatTime(raw, f)
	case "datetime":
		return FormatDateTime(raw, f)
	}
	return "unknown kind " + v.Kind
}

// INVARIANT: the shared golden vectors pass in Go. The SAME file is run by
// sdk/ts/admin-runtime's valueformat.test.ts, which is what makes "the two
// runtimes format a value identically" a checked property rather than a hope
// (docs/specs/i18n/formatting.md — the reason the table is explicit instead of
// delegated to Intl / x/text).
func TestFormatGoldenVectors(t *testing.T) {
	for _, v := range loadFormatVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			if got := applyVector(v); got != v.Want {
				t.Errorf("%s [%s/%s] = %q, want %q", v.Name, v.Locale, v.Kind, got, v.Want)
			}
		})
	}
}

// INVARIANT: every vector names a kind the dispatch knows — a typo'd kind must
// fail loudly here rather than silently assert the fallback string.
func TestFormatVectorsUseKnownKinds(t *testing.T) {
	known := map[string]bool{
		"number": true, "decimal": true, "percent": true,
		"date": true, "short_date": true, "time": true, "datetime": true,
	}
	for _, v := range loadFormatVectors(t) {
		if !known[v.Kind] {
			t.Errorf("vector %q uses unknown kind %q", v.Name, v.Kind)
		}
		if v.Value == nil && v.Float == nil {
			t.Errorf("vector %q sets neither value nor float", v.Name)
		}
		if v.Value != nil && v.Float != nil {
			t.Errorf("vector %q sets both value and float", v.Name)
		}
	}
}

// INVARIANT: the frozen table is complete and internally consistent. A new
// locale row that forgets a field would otherwise format as an empty
// separator, which reads as "no grouping" rather than as a mistake.
func TestFormatTableRowsAreComplete(t *testing.T) {
	locales := FormatLocales()
	if len(locales) < 2 {
		t.Fatalf("table carries %d locales, expected the shipped set", len(locales))
	}
	for _, l := range locales {
		f := ResolveFormats(l)
		if f.DecimalSeparator == "" {
			t.Errorf("%s: empty decimal_separator", l)
		}
		if f.ThousandSeparator == "" {
			t.Errorf("%s: empty thousand_separator", l)
		}
		if f.Grouping <= 0 {
			t.Errorf("%s: grouping = %d, want a positive size", l, f.Grouping)
		}
		if f.DecimalSeparator == f.ThousandSeparator {
			t.Errorf("%s: decimal and thousand separator are both %q", l, f.DecimalSeparator)
		}
		if f.DateFormat == "" || f.ShortDateFormat == "" || f.TimeFormat == "" {
			t.Errorf("%s: a date/time pattern is empty: %+v", l, f)
		}
		if f.DateTimeFormat == "" {
			t.Errorf("%s: empty datetime_format", l)
		}
		if f.PercentFormat == "" {
			t.Errorf("%s: empty percent_format", l)
		}
		if f.FirstDayOfWeek != 0 && f.FirstDayOfWeek != 1 {
			t.Errorf("%s: first_day_of_week = %d, want 0 (Sunday) or 1 (Monday)", l, f.FirstDayOfWeek)
		}
	}
}

// INVARIANT: every table pattern uses only the closed token vocabulary, and
// every percent/datetime shape carries its placeholder. A row whose pattern
// says "YYY" would silently render "YY" + a literal "Y".
func TestFormatTablePatternsAreRenderable(t *testing.T) {
	probe := CivilTime{Year: 2026, Month: 7, Day: 26, Hour: 14, Minute: 3, Second: 9}
	for _, l := range FormatLocales() {
		f := ResolveFormats(l)
		for _, p := range []string{f.DateFormat, f.ShortDateFormat, f.TimeFormat} {
			// The meridiem is a rendered VALUE containing letters, so strip it
			// before looking for un-substituted pattern letters.
			out := strings.NewReplacer("AM", "", "PM", "").Replace(RenderTimePattern(p, probe))
			for _, bad := range []string{"Y", "M", "D", "H", "h", "m", "s"} {
				if strings.Contains(out, bad) {
					t.Errorf("%s: pattern %q rendered %q, which still holds %q — unknown token?", l, p, out, bad)
				}
			}
		}
		if !strings.Contains(f.PercentFormat, "{value}") {
			t.Errorf("%s: percent_format %q has no {value} slot", l, f.PercentFormat)
		}
		if !strings.Contains(f.DateTimeFormat, "{date}") || !strings.Contains(f.DateTimeFormat, "{time}") {
			t.Errorf("%s: datetime_format %q must carry both {date} and {time}", l, f.DateTimeFormat)
		}
	}
}

// INVARIANT: the resolution chain from formatting.md §2 — an exact row, then
// the base language, then `en`.
func TestResolveFormatsChain(t *testing.T) {
	cases := []struct{ locale, wantDecimalSep, why string }{
		{"cs", ",", "exact row"},
		{"en-US", ".", "exact regional row"},
		{"cs-CZ", ",", "base-language fallback"},
		{"pt-BR", ",", "base-language fallback to pt"},
		{"xx", ".", "unknown locale falls back to en"},
		{"", ".", "empty locale falls back to en"},
	}
	for _, c := range cases {
		if got := ResolveFormats(c.locale).DecimalSeparator; got != c.wantDecimalSep {
			t.Errorf("ResolveFormats(%q).DecimalSeparator = %q, want %q (%s)",
				c.locale, got, c.wantDecimalSep, c.why)
		}
	}
}

// INVARIANT: the override map remaps ONE kind and leaves the rest on the
// language's own row — "Czech UI, English-looking numbers, Czech dates".
func TestResolveFormatsKindOverride(t *testing.T) {
	over := Overrides{"cs": {KindNumber: "en"}}

	if got := ResolveFormatsKind("cs", KindNumber, over).ThousandSeparator; got != "," {
		t.Errorf("cs/number thousand separator = %q, want the en comma", got)
	}
	if got := ResolveFormatsKind("cs", KindDate, over).DateFormat; got != "DD.MM.YYYY" {
		t.Errorf("cs/date format = %q, want the untouched cs pattern", got)
	}
	// A nil map is the identity mapping.
	if got := ResolveFormatsKind("cs", KindNumber, nil).ThousandSeparator; got != " " {
		t.Errorf("cs/number with no overrides = %q, want the cs space", got)
	}
	// An override naming a locale with no row must not break the runtime —
	// the lock parser rejects it; formatting falls through to the chain.
	bad := Overrides{"cs": {KindNumber: "zz"}}
	if got := ResolveFormatsKind("cs", KindNumber, bad).ThousandSeparator; got != " " {
		t.Errorf("cs/number with a bogus override = %q, want the cs row", got)
	}
}

// INVARIANT: the format locale is its own metadata key, and it DEFAULTS to the
// UI language rather than to a global constant.
func TestFormatLocaleFromContext(t *testing.T) {
	t.Cleanup(Reset)

	ctxWith := func(kv ...string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs(kv...))
	}

	if got := FormatLocaleFromContext(ctxWith(FormatLocaleMetadataKey, "cs")); got != "cs" {
		t.Errorf("explicit format locale = %q, want cs", got)
	}
	if got := FormatLocaleFromContext(ctxWith(LanguageMetadataKey, "de")); got != "de" {
		t.Errorf("absent format locale should fall back to w17-language, got %q", got)
	}
	// The format locale wins when both are present — an English UI with
	// Czech-looking values is the case the key exists for.
	both := ctxWith(LanguageMetadataKey, "en", FormatLocaleMetadataKey, "cs")
	if got := FormatLocaleFromContext(both); got != "cs" {
		t.Errorf("format locale should win over the UI language, got %q", got)
	}
	// Last write wins, matching w17-language's precedence model.
	multi := metadata.NewIncomingContext(context.Background(),
		metadata.MD{FormatLocaleMetadataKey: []string{"de", "cs"}})
	if got := FormatLocaleFromContext(multi); got != "cs" {
		t.Errorf("last value should win, got %q", got)
	}
	// No signal at all → the configured default language.
	if got := FormatLocaleFromContext(context.Background()); got != DefaultLanguage() {
		t.Errorf("bare ctx = %q, want the default language %q", got, DefaultLanguage())
	}
	// An empty value is not a signal.
	if got := FormatLocaleFromContext(ctxWith(FormatLocaleMetadataKey, "", LanguageMetadataKey, "fr")); got != "fr" {
		t.Errorf("empty format locale should fall through to fr, got %q", got)
	}
}

// INVARIANT: a raw q-weighted Accept-Language list — which the gateway's
// header rename passes through verbatim — is negotiated down to one tag the
// table knows, instead of missing every row and formatting as en.
func TestFormatLocaleFromContextNegotiatesAcceptLanguage(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(LanguageMetadataKey, "fr-FR,fr;q=0.9,cs;q=0.8"))
	got := FormatLocaleFromContext(ctx)
	if ResolveFormats(got).ThousandSeparator != " " || ResolveFormats(got).DecimalSeparator != "," {
		t.Errorf("negotiated locale %q does not format as French", got)
	}
	// A list whose every tag is unknown still resolves (to en, via the chain).
	unknown := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(LanguageMetadataKey, "zz-ZZ;q=0.9,qq;q=0.8"))
	if got := ResolveFormats(FormatLocaleFromContext(unknown)).DecimalSeparator; got != "." {
		t.Errorf("unknown list should fall back to en, got separator %q", got)
	}
}

// INVARIANT: HasFormatLocale answers about the TABLE, with no fallback walking
// — it is what the lock parser validates a declaration against, so `cs-CZ`
// (which formats fine via fallback) is still not a declarable row.
func TestHasFormatLocale(t *testing.T) {
	for _, l := range []string{"cs", "en", "en-US", "CS", "cs_CZ_", ""} {
		want := l == "cs" || l == "en" || l == "en-US" || l == "CS"
		if got := HasFormatLocale(l); got != want {
			t.Errorf("HasFormatLocale(%q) = %v, want %v", l, got, want)
		}
	}
	if HasFormatLocale("cs-CZ") {
		t.Error("cs-CZ has no row of its own; it formats via the base-language fallback")
	}
}

// INVARIANT: FormatKinds is the closed set the lock parser validates against.
func TestFormatKinds(t *testing.T) {
	kinds := FormatKinds()
	if len(kinds) != 6 {
		t.Fatalf("FormatKinds() = %v, want the six documented kinds", kinds)
	}
	seen := map[FormatKind]bool{}
	for _, k := range kinds {
		if seen[k] {
			t.Errorf("duplicate kind %q", k)
		}
		seen[k] = true
	}
	for _, want := range []FormatKind{KindNumber, KindDecimal, KindPercent, KindDate, KindTime, KindDateTime} {
		if !seen[want] {
			t.Errorf("FormatKinds() is missing %q", want)
		}
	}
}

// INVARIANT: grouping is driven by the row's size, not hardcoded to three.
func TestGroupDigitsRespectsSize(t *testing.T) {
	f := Formats{DecimalSeparator: ",", ThousandSeparator: "_", Grouping: 4}
	if got := FormatNumber("123456789", f); got != "1_2345_6789" {
		t.Errorf("grouping 4 = %q, want 1_2345_6789", got)
	}
	// A row with no grouping renders the digits bare.
	none := Formats{DecimalSeparator: ",", ThousandSeparator: "", Grouping: 3}
	if got := FormatNumber("123456789", none); got != "123456789" {
		t.Errorf("empty separator = %q, want the bare digits", got)
	}
}
