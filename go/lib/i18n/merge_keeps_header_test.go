package i18n_test

import (
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// A regen merges new msgids into a translated catalog. What the translator
// wrote besides the msgstrs — the header fields of their tool (Plural-Forms,
// Last-Translator, Language-Team, PO-Revision-Date) and their own comments
// on an entry — must survive the merge too.
func TestMergePO_KeepsTheTranslatorsHeaderAndComments(t *testing.T) {
	existing := `msgid ""
msgstr ""
"Project-Id-Version: w17\n"
"PO-Revision-Date: 2026-11-01 10:00+0100\n"
"Last-Translator: Jana <jana@example.com>\n"
"Language-Team: Czech\n"
"Language: cs\n"
"MIME-Version: 1.0\n"
"Content-Type: text/plain; charset=UTF-8\n"
"Content-Transfer-Encoding: 8bit\n"
"Plural-Forms: nplurals=3; plural=(n==1) ? 0 : (n>=2 && n<=4) ? 1 : 2;\n"

# keep it short: the button is narrow
#: forms.go:12
msgid "Save"
msgstr "Uložit"

# obsolete one's comment
msgid "Gone"
msgstr "Pryč"
`
	out, err := i18n.MergePO([]byte(existing), "cs", []string{"Save", "Cancel"})
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	for _, want := range []string{
		"Last-Translator: Jana <jana@example.com>",
		"Language-Team: Czech",
		"PO-Revision-Date: 2026-11-01 10:00+0100",
		"Plural-Forms: nplurals=3;",
		"# keep it short: the button is narrow\n",
		`msgstr "Uložit"`,
		`msgid "Cancel"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("merged catalog lost %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "obsolete one's comment") {
		t.Errorf("an obsolete entry's comment survived:\n%s", body)
	}
	if i, j := strings.Index(body, "# keep it short"), strings.Index(body, `msgid "Save"`); i < 0 || i > j {
		t.Errorf("the comment is not before its entry:\n%s", body)
	}
	if err := i18n.ValidatePO(out); err != nil {
		t.Errorf("the merged catalog does not validate: %v\n%s", err, body)
	}
}
