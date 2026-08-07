package i18n_test

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/sdk/go/lib/i18n"
)

// minimalPO produces a tiny valid .po body translating one
// msgid → msgstr under the supplied language. The header is
// the bare minimum gotext needs to consider the file well-
// formed (Content-Type charset, MIME-Version, Language tag).
func minimalPO(lang, msgid, msgstr string) string {
	return `msgid ""
msgstr ""
"Content-Type: text/plain; charset=UTF-8\n"
"MIME-Version: 1.0\n"
"Language: ` + lang + `\n"

msgid "` + msgid + `"
msgstr "` + msgstr + `"
`
}

// resetState clears any in-process catalog state so tests
// don't pollute each other.
func resetState(t *testing.T) {
	t.Helper()
	i18n.Reset()
}

// TestT_DefaultLocaleFallback — no catalog registered, T
// returns the msgid verbatim (with placeholder substitution).
func TestT_DefaultLocaleFallback(t *testing.T) {
	resetState(t)
	got := i18n.T(context.Background(), "must be at most {max} characters",
		map[string]string{"max": "10"})
	want := "must be at most 10 characters"
	if got != want {
		t.Errorf("T(...) = %q, want %q", got, want)
	}
}

// TestT_RegisteredLocale_TranslatesAndSubstitutes — register a
// catalog under "cs", request via w17-language metadata,
// translated string comes back + placeholder substituted.
func TestT_RegisteredLocale_TranslatesAndSubstitutes(t *testing.T) {
	resetState(t)
	i18n.Register("cs", []byte(minimalPO("cs",
		"must be at most {max} characters",
		"musí být nejvýše {max} znaků")))

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, "cs"))

	got := i18n.T(ctx, "must be at most {max} characters",
		map[string]string{"max": "8"})
	want := "musí být nejvýše 8 znaků"
	if got != want {
		t.Errorf("T(cs) = %q, want %q", got, want)
	}
}

// B31-i18n-1: w17-language often carries a raw Accept-Language header (the
// Accept-Language → w17-language rename), which browsers ALWAYS send as a
// multi-value q-weighted list (e.g. "fr-FR,fr;q=0.9,cs;q=0.8"). An exact catalog
// lookup on that whole string misses → every browser request fell back to the
// default language. T must negotiate: parse the list (q-order) and pick the
// highest-priority tag that has a catalog (exact or base-language).
func TestT_AcceptLanguageMultiValue_NegotiatesCatalog_B31(t *testing.T) {
	resetState(t)
	i18n.Register("cs", []byte(minimalPO("cs", "hello", "ahoj")))
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, "fr-FR,fr;q=0.9,cs;q=0.8"))
	if got := i18n.T(ctx, "hello", nil); got != "ahoj" {
		t.Errorf("T(Accept-Language) = %q, want \"ahoj\" (cs via negotiation, not default)", got)
	}
}

func TestT_AcceptLanguageRegion_FallsBackToBase_B31(t *testing.T) {
	resetState(t)
	i18n.Register("en", []byte(minimalPO("en", "hello", "hi")))
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, "en-US,en;q=0.9"))
	if got := i18n.T(ctx, "hello", nil); got != "hi" {
		t.Errorf("T(en-US,…) = %q, want \"hi\" (en base catalog)", got)
	}
}

// B6-i18n-1: T with an EMPTY msgid must return "" — never the .po header.
// In gettext the empty msgid ("") is reserved for the catalog's header
// metadata (Content-Type, Language, …), so an unguarded lookupCatalog("")
// returns that header. A caller translating a possibly-empty label would then
// leak ".po" header text into a user-facing string.
func TestT_EmptyMsgid_ReturnsEmpty(t *testing.T) {
	resetState(t)
	i18n.Register("cs", []byte(minimalPO("cs", "hello", "ahoj")))
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, "cs"))
	if got := i18n.T(ctx, "", nil); got != "" {
		t.Errorf("T(empty msgid) = %q, want \"\" (must not leak the .po header)", got)
	}
}

// TestT_RequestedLocaleMissing_FallsBackToDefault — when the
// requested locale isn't registered, T uses the default
// locale's catalog. With default=en and en registered, "cs"
// in metadata falls back to the EN translation.
func TestT_RequestedLocaleMissing_FallsBackToDefault(t *testing.T) {
	resetState(t)
	i18n.Register("en", []byte(minimalPO("en",
		"is required",
		"is required (EN catalog)")))

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, "cs"))

	got := i18n.T(ctx, "is required", nil)
	want := "is required (EN catalog)"
	if got != want {
		t.Errorf("T(cs, no-catalog) = %q, want %q (default-en fallback)", got, want)
	}
}

// TestT_NoCatalogForDefaultLanguage_ReturnsMsgid — when even
// the default locale has no catalog, T returns the msgid
// verbatim (with substitution applied).
func TestT_NoCatalogForDefaultLanguage_ReturnsMsgid(t *testing.T) {
	resetState(t)
	got := i18n.T(context.Background(), "is required", nil)
	want := "is required"
	if got != want {
		t.Errorf("T = %q, want %q (bare msgid passthrough)", got, want)
	}
}

// TestT_SetDefaultLanguage_FlipsFallback — SetDefaultLanguage
// makes the named locale the new fallback target when the
// requested locale is missing.
func TestT_SetDefaultLanguage_FlipsFallback(t *testing.T) {
	resetState(t)
	i18n.Register("cs", []byte(minimalPO("cs",
		"is required",
		"je požadováno")))
	i18n.SetDefaultLanguage("cs")

	got := i18n.T(context.Background(), "is required", nil)
	want := "je požadováno"
	if got != want {
		t.Errorf("T (default=cs) = %q, want %q", got, want)
	}
}

// TestLocaleFromContext_LastValueWins — when multiple w17-language
// values land on the metadata, the LAST one wins.
//
// One convention, decided rather than inherited. The gateway's writers
// APPEND, and the one that appends LAST is the most specific: the
// per-endpoint `metadata_bindings[]` overlay fires inside the generated
// handler, i.e. after every middleware, so under a first-wins reader it
// could never override anything — the feature would be inert by
// construction. The `default_language` stamp, the least specific writer,
// is guarded: [restgw.MetadataDefaultStampMiddleware] stamps only a key
// no upstream layer populated, and `metadata.FromOutgoingContext` DOES
// merge pairs added via `AppendToOutgoingContext` (grpc-go v1.82.1,
// metadata.go — the `raw.added` merge loop), so the fallback can never
// land on top of a rename.
//
// Between 2026-07-31 and this fix the function read vals[0] on the
// premise that the stamp guard was blind to appended pairs. It is not,
// and first-wins left `FormatLocaleFromContext`, the restgw package doc
// and the emitted handler comment all stating the opposite convention.
// See TestLocaleReaders_AgreeOnPrecedence.
func TestLocaleFromContext_LastValueWins(t *testing.T) {
	resetState(t)
	md := metadata.MD{}
	md.Append(i18n.LanguageMetadataKey, "de")
	md.Append(i18n.LanguageMetadataKey, "en")
	md.Append(i18n.LanguageMetadataKey, "cs")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	got := i18n.LocaleFromContext(ctx)
	if got != "cs" {
		t.Errorf("LocaleFromContext = %q, want %q (last wins)", got, "cs")
	}
}

// TestLocaleReaders_AgreeOnPrecedence pins the property the two keys'
// readers lost: `w17-language` and `w17-format-locale` travel the SAME
// middleware chain, so a value at the same position in each must win in
// both. Asserting each reader's own index separately is what let them
// drift apart — one read vals[0], the other vals[len-1], and every doc
// describing "the precedence chain" was true of exactly one of them.
func TestLocaleReaders_AgreeOnPrecedence(t *testing.T) {
	resetState(t)
	md := metadata.MD{}
	for _, v := range []string{"de", "en", "cs"} {
		md.Append(i18n.LanguageMetadataKey, v)
		md.Append(i18n.FormatLocaleMetadataKey, v)
	}
	ctx := metadata.NewIncomingContext(context.Background(), md)

	lang := i18n.LocaleFromContext(ctx)
	format := i18n.FormatLocaleFromContext(ctx)
	if lang != format {
		t.Errorf("the two locale keys resolve different positions of one appended chain: "+
			"LocaleFromContext = %q, FormatLocaleFromContext = %q — one convention, both readers", lang, format)
	}
}

// TestLocaleFromContext_EmptyMetadata_ReturnsDefault — no
// metadata key → configured default.
func TestLocaleFromContext_EmptyMetadata_ReturnsDefault(t *testing.T) {
	resetState(t)
	if got := i18n.LocaleFromContext(context.Background()); got != "en" {
		t.Errorf("LocaleFromContext (empty) = %q, want %q", got, "en")
	}
	i18n.SetDefaultLanguage("cs")
	if got := i18n.LocaleFromContext(context.Background()); got != "cs" {
		t.Errorf("LocaleFromContext (default=cs) = %q, want %q", got, "cs")
	}
}

// TestRegisterFS_WalksAndRegisters — RegisterFS walks the
// filesystem, finds every *.po, registers under filename-
// derived lang tags. Returns the list.
func TestRegisterFS_WalksAndRegisters(t *testing.T) {
	resetState(t)
	mfs := fstest.MapFS{
		"i18n/en.po":     &fstest.MapFile{Data: []byte(minimalPO("en", "ok", "ok-en"))},
		"i18n/cs.po":     &fstest.MapFile{Data: []byte(minimalPO("cs", "ok", "ok-cs"))},
		"i18n/README.md": &fstest.MapFile{Data: []byte("ignored")},
	}
	langs, err := i18n.RegisterFS(mfs)
	if err != nil {
		t.Fatalf("RegisterFS: %v", err)
	}
	if len(langs) != 2 {
		t.Errorf("RegisterFS: registered %d languages (%v), want 2", len(langs), langs)
	}

	// Each registered language resolves its msgid.
	for _, lang := range []string{"en", "cs"} {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(i18n.LanguageMetadataKey, lang))
		want := "ok-" + lang
		if got := i18n.T(ctx, "ok", nil); got != want {
			t.Errorf("T(%s) = %q, want %q", lang, got, want)
		}
	}
}

// TestRegisterFS_PropagatesReadError — surface filesystem
// errors so callers don't silently miss a missing embed.
func TestRegisterFS_PropagatesReadError(t *testing.T) {
	resetState(t)
	failing := failingFS{}
	_, err := i18n.RegisterFS(failing)
	if err == nil {
		t.Error("RegisterFS: expected error from failing fs, got nil")
	}
}

// TestSubstitute_UnknownPlaceholderPassesThrough — a `{foo}`
// the translator added that we don't supply should NOT panic
// or strip; passes through verbatim so the bug shows.
func TestSubstitute_UnknownPlaceholderPassesThrough(t *testing.T) {
	resetState(t)
	got := i18n.T(context.Background(), "value {known} but also {unknown}",
		map[string]string{"known": "x"})
	want := "value x but also {unknown}"
	if got != want {
		t.Errorf("T = %q, want %q", got, want)
	}
}

// TestSetDefaultLanguage_IgnoresEmpty — defensive: an empty
// string from a bad config doesn't clobber the default.
func TestSetDefaultLanguage_IgnoresEmpty(t *testing.T) {
	resetState(t)
	i18n.SetDefaultLanguage("cs")
	i18n.SetDefaultLanguage("")
	if got := i18n.DefaultLanguage(); got != "cs" {
		t.Errorf("DefaultLanguage = %q, want %q (empty ignored)", got, "cs")
	}
}

// --- helpers ---

// failingFS implements fs.FS by always returning an error,
// used to assert RegisterFS propagates the failure.
type failingFS struct{}

func (failingFS) Open(name string) (fs.File, error) {
	return nil, fs.ErrInvalid
}

// TestLocaleFromContext_OutgoingMetadata — a GATEWAY has no incoming
// metadata: it is the side ATTACHING metadata for the hop it is about to
// make. Its own request-validation block renders before any backend call,
// so the locale it must honour is on the outgoing side.
//
// Reading incoming only sent every gateway-rendered message straight to
// the surface default, ignoring the caller's Accept-Language — and that
// was invisible until a surface declared `default_language`, because
// until then the fallback and the answer were the same string.
func TestLocaleFromContext_OutgoingMetadata(t *testing.T) {
	resetState(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), i18n.LanguageMetadataKey, "de")
	if got := i18n.LocaleFromContext(ctx); got != "de" {
		t.Errorf("LocaleFromContext = %q, want %q (outgoing metadata)", got, "de")
	}
}

// Incoming outranks outgoing: a backend that also happens to be
// attaching metadata for a downstream call must answer in the locale it
// was CALLED with, not the one it is about to forward.
func TestLocaleFromContext_IncomingBeatsOutgoing(t *testing.T) {
	resetState(t)
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(i18n.LanguageMetadataKey, "cs"))
	ctx = metadata.AppendToOutgoingContext(ctx, i18n.LanguageMetadataKey, "de")
	if got := i18n.LocaleFromContext(ctx); got != "cs" {
		t.Errorf("LocaleFromContext = %q, want %q (incoming wins)", got, "cs")
	}
}
