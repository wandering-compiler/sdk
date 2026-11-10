package i18n

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
)

// A `#, fuzzy` entry is a translator's unreviewed guess — gettext's msgfmt
// leaves it out of the compiled catalog, so it is never served. The runtime
// registers the project's .po files directly, and the parser ignores `#,`
// flags: the guess was served as the translation, ValidatePO said nothing,
// and the next MergePO dropped the flag, turning the guess into a reviewed
// translation for good.
const fuzzyPO = "msgid \"\"\nmsgstr \"\"\n\"Content-Type: text/plain; charset=UTF-8\\n\"\n\n" +
	"#: src/a.go:1\n#, fuzzy, python-format\nmsgid \"bye\"\nmsgstr \"STALE\"\n\n" +
	"#, fuzzy\nmsgid \"\"\n\"long \"\n\"one\"\nmsgstr \"STALE2\"\n\n" +
	"msgid \"hello\"\nmsgstr \"ahoj\"\n"

func TestFuzzyEntryIsNotServed(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	_ = RegisterCatalog("cs", []byte(fuzzyPO))
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("w17-language", "cs"))
	if got := T(ctx, "bye", nil); got != "bye" {
		t.Errorf("T(cs, bye) = %q, want the msgid: a fuzzy entry is untranslated", got)
	}
	if got := T(ctx, "long one", nil); got != "long one" {
		t.Errorf("T(cs, long one) = %q, want the msgid", got)
	}
	if got := T(ctx, "hello", nil); got != "ahoj" {
		t.Errorf("T(cs, hello) = %q, want the reviewed translation", got)
	}
}

func TestValidatePO_ReportsFuzzyEntries(t *testing.T) {
	err := ValidatePO([]byte(fuzzyPO))
	if err == nil {
		t.Fatal("ValidatePO said nothing about two fuzzy entries")
	}
	for _, want := range []string{`line 7: entry "bye" is marked fuzzy`, `line 11: entry "long one" is marked fuzzy`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidatePO = %v, want it to contain %q", err, want)
		}
	}
	// A fuzzy header (msginit writes one) is not an entry anyone looks up.
	if err := ValidatePO([]byte("#, fuzzy\nmsgid \"\"\nmsgstr \"\"\n\"Language: cs\\n\"\n\nmsgid \"a\"\nmsgstr \"b\"\n")); err != nil {
		t.Errorf("ValidatePO of a fuzzy header = %v, want nil", err)
	}
}

func TestMergePO_KeepsTheFuzzyFlag(t *testing.T) {
	merged, err := MergePO([]byte(fuzzyPO), "cs", []string{"bye", "hello", "new"})
	if err != nil {
		t.Fatal(err)
	}
	out := string(merged)
	i := strings.Index(out, "msgid \"bye\"")
	if i < 0 || !strings.Contains(out[:i], "#, fuzzy") {
		t.Fatalf("MergePO dropped the fuzzy flag of \"bye\":\n%s", out)
	}
	if !strings.Contains(out, "msgstr \"STALE\"") {
		t.Errorf("MergePO dropped the translator's suggestion:\n%s", out)
	}
	// The merged catalog still does not serve it, and still flags exactly it.
	verr := ValidatePO(merged)
	if verr == nil || !strings.Contains(verr.Error(), `entry "bye" is marked fuzzy`) || strings.Contains(verr.Error(), `"hello"`) {
		t.Errorf("ValidatePO(merged) = %v, want exactly the bye entry flagged", verr)
	}
	for _, e := range EntriesFromPO(merged) {
		if e.Msgid == "bye" && (!e.Fuzzy || e.Msgstr != "") {
			t.Errorf("EntriesFromPO bye = %+v, want Fuzzy with no msgstr (a client catalog must not serve it)", e)
		}
		if e.Msgid == "hello" && (e.Fuzzy || e.Msgstr != "ahoj") {
			t.Errorf("EntriesFromPO hello = %+v", e)
		}
	}
}
