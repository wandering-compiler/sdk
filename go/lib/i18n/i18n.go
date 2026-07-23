// Package i18n is the runtime gettext catalog the wandering-
// compiler's generated bundles use to localize user-facing
// strings (REV-149).
//
// The package owns three concerns:
//
//  1. Catalog registration — generated init code calls
//     [Register] once per supported language with the embedded
//     `.po` file bytes (each domain binary carries every
//     lock-declared language's catalog via `embed.FS`).
//
//  2. Per-request locale resolution — [LocaleFromContext]
//     reads the LAST value of the `w17-language` gRPC
//     metadata key from the incoming ctx, honoring the
//     last-write-wins precedence the gateway middleware
//     chain establishes (default stamp → header rename →
//     metadata_bindings → cap call).
//
//  3. Message lookup + placeholder substitution — [T] is the
//     one function generated handlers call. It picks the
//     requested locale's catalog, falls back to the default
//     locale, then to the bare msgid; substitutes `{name}`
//     tokens from the supplied params map.
//
// Plurals, msgctxt context-keyed translations, and locale-
// aware number / date formatting are out of scope for iter-1.
// Each can lift in a follow-up REV when a fixture demands it
// (gotext supports them natively — the wiring is just absent
// here).
//
// Concurrency: every exported function is safe for concurrent
// use. The catalog map is guarded by an RWMutex; reads
// dominate at request time so the lock is read-mostly.
package i18n

import (
	"context"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/leonelquinteros/gotext"
	"golang.org/x/text/language"
	"google.golang.org/grpc/metadata"
)

// LanguageMetadataKey is the gRPC metadata key carrying the
// per-request locale. Convention is lowercase ASCII per the
// gRPC convention (mixed-case silently lowercases at runtime).
const LanguageMetadataKey = "w17-language"

// CompileTimeDefaultLanguage is the locale every binary
// starts with. Generated binaries whose surface declares a
// non-EN `default_language` call [SetDefaultLanguage] at init
// to override.
const CompileTimeDefaultLanguage = "en"

var (
	catalogsMu  sync.RWMutex
	catalogs    = map[string]*gotext.Po{}
	defaultLang = CompileTimeDefaultLanguage
)

// SetDefaultLanguage overrides the compile-time default ("en")
// locale used when the request carries no `w17-language`
// metadata signal AND no catalog is registered for the
// requested locale. Called by generated init code when a
// RestApi surface declares a non-EN default_language.
//
// Safe to call concurrently with [T] / [Register] / [Reset].
func SetDefaultLanguage(lang string) {
	if lang == "" {
		return
	}
	catalogsMu.Lock()
	defaultLang = lang
	catalogsMu.Unlock()
}

// DefaultLanguage returns the currently configured default
// locale. Reflects [SetDefaultLanguage] overrides; reads from
// the [CompileTimeDefaultLanguage] constant when unmodified.
func DefaultLanguage() string {
	catalogsMu.RLock()
	defer catalogsMu.RUnlock()
	return defaultLang
}

// Register parses the supplied `.po` file bytes and stores
// the catalog under `lang`. Replaces any existing catalog
// registered under the same language tag (last call wins).
//
// Empty `lang` is a no-op (the caller is expected to pass a
// concrete BCP-47 tag). Empty `poContents` registers an empty
// catalog — every lookup falls through to default-locale or
// msgid.
//
// Safe to call concurrently with [T]; treat as init-time
// configuration in practice.
func Register(lang string, poContents []byte) {
	if lang == "" {
		return
	}
	po := gotext.NewPo()
	po.Parse(poContents)
	catalogsMu.Lock()
	catalogs[lang] = po
	catalogsMu.Unlock()
}

// RegisterFS walks `filesystem` looking for `*.po` files at
// any depth + registers each one under the language tag
// derived from the filename (without `.po`). Returns the list
// of registered languages in walk order (typically lexical).
//
// Convention matches what the codegen pipeline emits:
//
//	i18n/en.po → Register("en", ...)
//	i18n/cs.po → Register("cs", ...)
//
// Nested directories are searched too — the base filename
// alone determines the language tag.
func RegisterFS(filesystem fs.FS) ([]string, error) {
	var langs []string
	err := fs.WalkDir(filesystem, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(p, ".po") {
			return nil
		}
		body, err := fs.ReadFile(filesystem, p)
		if err != nil {
			return err
		}
		base := path.Base(p)
		lang := strings.TrimSuffix(base, ".po")
		Register(lang, body)
		langs = append(langs, lang)
		return nil
	})
	return langs, err
}

// Reset clears every registered catalog AND restores the
// default language to [CompileTimeDefaultLanguage]. Useful in
// tests; production code rarely calls this.
func Reset() {
	catalogsMu.Lock()
	catalogs = map[string]*gotext.Po{}
	defaultLang = CompileTimeDefaultLanguage
	catalogsMu.Unlock()
}

// T resolves `msgid` to its localized form for the locale
// carried by ctx's `w17-language` gRPC metadata, then
// substitutes `{name}` placeholders from `params`.
//
// Resolution order:
//
//  1. Catalog for the requested locale (last value of
//     w17-language metadata).
//  2. Catalog for the configured default locale.
//  3. Bare msgid (unmodified English template).
//
// In every case the result passes through placeholder
// substitution before return. Placeholders absent from
// `params` survive verbatim (so a translator who added a
// `{foo}` we don't pass doesn't crash).
//
// Concurrent-safe; the catalog map is read-locked.
func T(ctx context.Context, msgid string, params map[string]string) string {
	lang := LocaleFromContext(ctx)
	cat := negotiateCatalog(lang)

	out := msgid
	// Guard the empty msgid: in gettext "" is the header entry, so
	// lookupCatalog("") returns the .po header metadata (Content-Type,
	// Language, …) rather than a translation — return "" instead of leaking
	// it (B6-i18n-1).
	if msgid != "" && cat != nil {
		if t := lookupCatalog(cat, msgid); t != "" {
			out = t
		}
	}
	return substitute(out, params)
}

// negotiateCatalog resolves the catalog for a `w17-language` value. The value is
// usually a clean tag ("cs") from a client that sets it directly, but the
// Accept-Language → w17-language rename can leave it a RAW, multi-value,
// q-weighted Accept-Language list ("fr-FR,fr;q=0.9,cs;q=0.8") — which browsers
// always send. B31-i18n-1: an exact lookup on that whole string missed and fell
// back to the default for every browser request. So:
//  1. fast path — exact match (the clean-tag case),
//  2. else parse as Accept-Language (q-order) and pick the highest-priority tag
//     that has a catalog, matching the tag exactly then by base language,
//  3. else the default catalog.
func negotiateCatalog(lang string) *gotext.Po {
	catalogsMu.RLock()
	defer catalogsMu.RUnlock()
	if cat, ok := catalogs[lang]; ok {
		return cat
	}
	if tags, _, err := language.ParseAcceptLanguage(lang); err == nil {
		for _, t := range tags {
			if cat, ok := catalogs[t.String()]; ok {
				return cat
			}
			if base, conf := t.Base(); conf != language.No {
				if cat, ok := catalogs[base.String()]; ok {
					return cat
				}
			}
		}
	}
	return catalogs[defaultLang]
}

// lookupCatalog wraps `cat.Get(msgid)` through a method-value
// indirection. gotext.Po.Get is signatured `(string, ...interface{})`
// and behaves printf-like when vars are supplied — go vet flags
// the dynamic msgid as a non-constant format string. Iter-1
// always passes zero vars (placeholder substitution is our own
// `{name}` concern, not gotext's `%v`-Sprintf concern), so the
// printf check has no real signal here. The method-value form
// satisfies vet without changing runtime behaviour.
func lookupCatalog(cat *gotext.Po, msgid string) string {
	get := cat.Get
	return get(msgid)
}

// LocaleFromContext returns the requested locale from ctx,
// reading the LAST value of `w17-language` gRPC metadata
// (since the gateway middleware uses last-write-wins
// precedence: default stamp → rename → metadata_bindings →
// cap). Returns the configured default when no signal is
// present.
//
// Reads `metadata.FromIncomingContext` only — outgoing
// metadata is the gateway's emit surface, not the storage
// handler's input.
func LocaleFromContext(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(LanguageMetadataKey); len(vals) > 0 {
			// Last value wins — matches the gateway's
			// middleware-layered precedence model.
			return vals[len(vals)-1]
		}
	}
	return DefaultLanguage()
}

// substitute walks `msg` replacing each `{name}` token with
// `params[name]`. Cheap early-out when the message has no
// `{` or params is empty. Unknown placeholder names pass
// through verbatim — a translation that referenced `{foo}`
// the caller didn't supply renders as literal `{foo}` rather
// than crashing.
//
// Iter-1 keeps the substitution naive (single pass, no
// nested-token handling, no quoting). Real-world validation
// messages stay simple shapes ("must be at most {max}
// characters") so this is enough; if a future fixture demands
// nested or escaping semantics, lift this layer.
func substitute(msg string, params map[string]string) string {
	if len(params) == 0 || !strings.ContainsRune(msg, '{') {
		return msg
	}
	out := msg
	for k, v := range params {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}
