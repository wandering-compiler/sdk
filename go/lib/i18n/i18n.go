// Package i18n is the runtime gettext catalog the wandering-
// compiler's generated bundles use to localize user-facing
// strings.
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
//     metadata key, honoring the last-write-wins precedence
//     the gateway's writers establish (auto-installed
//     Accept-Language rename → authored header renames →
//     per-endpoint metadata_bindings; the default stamp
//     fills the key only when none of them wrote it).
//
//  3. Message lookup + placeholder substitution — [T] is the
//     one function generated handlers call. It picks the
//     requested locale's catalog, falls back to the default
//     locale, then to the bare msgid (per message); substitutes `{name}`
//     tokens from the supplied params map.
//
// Plurals, msgctxt context-keyed translations, and locale-
// aware number / date formatting are not supported yet.
// gotext supports them natively; the wiring is just absent
// here.
//
// Catalogs and the default language are process-global: one
// process carries one language policy, and the per-request
// locale arrives in metadata. Registering a second catalog for
// a language merges it into the first; a conflicting
// translation or a second, different default language is
// reported (see [RegisterCatalog], [SetDefaultLanguage]).
//
// Concurrency: every exported function is safe for concurrent
// use. The catalog map is guarded by an RWMutex; reads
// dominate at request time so the lock is read-mostly.
package i18n

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
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
	catalogsMu sync.RWMutex
	// catalogs maps a normalised language tag (see [normalizeTag]) to its
	// flattened msgid → msgstr table. Only translated entries are kept, so
	// presence in the map is what "the catalog has this message" means.
	catalogs = map[string]map[string]string{}
	// defaultLang is the configured default as given; defaultSet records
	// whether SetDefaultLanguage has been called since the last Reset, so a
	// second, different value can be reported as a conflict.
	defaultLang = CompileTimeDefaultLanguage
	defaultSet  bool
)

// SetDefaultLanguage overrides the compile-time default ("en")
// locale used when the request carries no `w17-language`
// metadata signal, and as the second step of the per-message
// fallback chain in [T]. Called by generated init code when a
// RestApi surface declares a non-EN default_language.
//
// The default is process-global. A process that composes two
// surfaces with different defaults cannot honour both; a second
// call with a different value still takes effect (last call
// wins) but is logged as a warning so the conflict is visible
// at boot instead of in a translated response.
//
// Safe to call concurrently with [T] / [Register] / [Reset].
func SetDefaultLanguage(lang string) {
	if lang == "" {
		return
	}
	catalogsMu.Lock()
	prev, wasSet := defaultLang, defaultSet
	defaultLang = lang
	defaultSet = true
	catalogsMu.Unlock()
	if wasSet && normalizeTag(prev) != normalizeTag(lang) {
		slog.Warn("i18n: default language set twice with different values; the last one wins for the whole process",
			"previous", prev, "now", lang)
	}
}

// DefaultLanguage returns the currently configured default
// locale. Reflects [SetDefaultLanguage] overrides; reads from
// the [CompileTimeDefaultLanguage] constant when unmodified.
func DefaultLanguage() string {
	catalogsMu.RLock()
	defer catalogsMu.RUnlock()
	return defaultLang
}

// Register parses the supplied `.po` file bytes and adds the
// catalog's translations under `lang`. It is [RegisterCatalog]
// with the problems logged instead of returned; see there for
// the merge and conflict rules.
//
// Empty `lang` is a no-op (the caller is expected to pass a
// concrete BCP-47 tag). Empty `poContents` registers an empty
// catalog — every lookup falls through to default-locale or
// msgid.
//
// Safe to call concurrently with [T]; treat as init-time
// configuration in practice.
func Register(lang string, poContents []byte) {
	if err := RegisterCatalog(lang, poContents); err != nil {
		slog.Warn("i18n: catalog registered with problems", "lang", lang, "error", err)
	}
}

// RegisterCatalog parses the supplied `.po` file bytes and adds the
// catalog's translations under `lang`, reporting what went wrong instead
// of hiding it:
//
//   - A malformed file (see [ValidatePO]) is still registered best-effort
//     — whatever entries parse keep serving — and the syntax error is
//     returned, so a broken catalog is visible at boot.
//   - Catalogs are process-global. Registering the same language twice
//     MERGES the two: messages only one of them translates are all kept,
//     so two surfaces composed into one process each keep their own
//     translations. When both translate the same msgid differently the
//     FIRST registration is kept and the conflict is returned.
//
// The language tag is matched without regard to case or `_`/`-` spelling,
// so a gettext-style `pt_BR` catalog serves a `pt-BR` request.
//
// Empty `lang` is a no-op and returns nil.
func RegisterCatalog(lang string, poContents []byte) error {
	if lang == "" {
		return nil
	}
	syntaxErr := ValidatePO(poContents)
	po := gotext.NewPo()
	po.Parse(poContents)
	entries := map[string]string{}
	for id, tr := range po.GetDomain().GetTranslations() {
		// "" is the header entry; an empty msgstr is "untranslated" in
		// gettext and must fall through like an absent entry.
		if id == "" {
			continue
		}
		if s := rawMsgstr(tr); s != "" {
			entries[id] = s
		}
	}

	key := normalizeTag(lang)
	var conflicts []string
	catalogsMu.Lock()
	cat, ok := catalogs[key]
	if !ok {
		cat = make(map[string]string, len(entries))
		catalogs[key] = cat
	}
	for id, s := range entries {
		if prev, ok := cat[id]; ok {
			if prev != s {
				conflicts = append(conflicts, id)
			}
			continue
		}
		cat[id] = s
	}
	catalogsMu.Unlock()

	var errs []error
	if syntaxErr != nil {
		errs = append(errs, fmt.Errorf("i18n: %s: malformed catalog (registered what parses): %w", lang, syntaxErr))
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		errs = append(errs, fmt.Errorf("i18n: %s: %d message(s) already registered with a different translation; the first registration is kept: %q",
			lang, len(conflicts), conflicts))
	}
	return errors.Join(errs...)
}

// RegisterFS walks `filesystem` looking for `*.po` files at
// any depth + registers each one under the language tag
// derived from the filename (without `.po`). Returns the list
// of registered languages in walk order (typically lexical).
//
// Convention matches what the codegen pipeline emits:
//
//	i18n/en.po    → Register("en", ...)
//	i18n/cs.po    → Register("cs", ...)
//	i18n/pt_BR.po → Register("pt_BR", ...), which serves "pt-BR"
//
// Nested directories are searched too — the base filename
// alone determines the language tag.
//
// A read error stops the walk and is returned. Problems that do not stop
// registration — a malformed catalog, conflicting translations (see
// [RegisterCatalog]) or a tree with no catalog at all — are joined into
// the returned error after every catalog has been registered, so a boot
// that logs the error names every broken file while still serving the
// good ones.
func RegisterFS(filesystem fs.FS) ([]string, error) {
	var langs []string
	var problems []error
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
		if err := RegisterCatalog(lang, body); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", p, err))
		}
		langs = append(langs, lang)
		return nil
	})
	if err != nil {
		return langs, err
	}
	if len(langs) == 0 {
		problems = append(problems, ErrNoCatalogs)
	}
	return langs, errors.Join(problems...)
}

// ErrNoCatalogs is returned (joined) by [RegisterFS] when the tree holds
// no `.po` file: every message will render as its msgid.
var ErrNoCatalogs = errors.New("i18n: no .po catalog found; messages render untranslated")

// Reset clears every registered catalog AND restores the
// default language to [CompileTimeDefaultLanguage]. Useful in
// tests; production code rarely calls this.
func Reset() {
	catalogsMu.Lock()
	catalogs = map[string]map[string]string{}
	defaultLang = CompileTimeDefaultLanguage
	defaultSet = false
	catalogsMu.Unlock()
}

// T resolves `msgid` to its localized form for the locale
// carried by ctx's `w17-language` gRPC metadata, then
// substitutes `{name}` placeholders from `params`.
//
// Resolution order, per message:
//
//  1. Catalog for the requested locale (last value of
//     w17-language metadata), if it translates msgid.
//  2. Catalog for the configured default locale, if it
//     translates msgid.
//  3. Bare msgid (unmodified English template).
//
// A partially translated catalog therefore falls back to the
// default language for the messages it lacks, not to the
// source text.
//
// In every case the result passes through placeholder
// substitution before return. Placeholders absent from
// `params` survive verbatim (so a translator who added a
// `{foo}` we don't pass doesn't crash).
//
// Concurrent-safe; the catalog map is read-locked.
func T(ctx context.Context, msgid string, params map[string]string) string {
	// Guard the empty msgid: in gettext "" is the header entry, so a lookup
	// of "" would return the .po header metadata (Content-Type, Language, …)
	// rather than a translation — return "" instead of leaking it.
	if msgid == "" {
		return ""
	}
	return substitute(lookup(LocaleFromContext(ctx), msgid), params)
}

// lookup walks the per-message chain: the negotiated catalog for `lang`,
// then the default-language catalog, then the msgid itself.
func lookup(lang, msgid string) string {
	catalogsMu.RLock()
	defer catalogsMu.RUnlock()
	if cat := negotiateCatalog(lang); cat != nil {
		if s, ok := cat[msgid]; ok {
			return s
		}
	}
	if cat := catalogs[normalizeTag(defaultLang)]; cat != nil {
		if s, ok := cat[msgid]; ok {
			return s
		}
	}
	return msgid
}

// negotiateCatalog resolves the catalog for a `w17-language` value, or nil
// when nothing matches (the caller then tries the default catalog). The
// value is usually a clean tag ("cs") from a client that sets it directly,
// but the Accept-Language → w17-language rename can leave it a RAW,
// multi-value, q-weighted Accept-Language list ("fr-FR,fr;q=0.9,cs;q=0.8")
// — which browsers always send. An exact lookup on that whole string would
// miss for every browser request. So:
//  1. fast path — exact match of the normalised tag (the clean-tag case),
//  2. else parse as Accept-Language (q-order) and pick the highest-priority
//     tag that has a catalog, matching the tag exactly then by base language.
//
// Tags are compared in [normalizeTag] form on both sides, so a catalog
// registered from `pt_BR.po` serves `pt-BR` and `PT-br` alike.
//
// Caller holds catalogsMu (read).
func negotiateCatalog(lang string) map[string]string {
	if cat, ok := catalogs[normalizeTag(lang)]; ok {
		return cat
	}
	if tags, _, err := language.ParseAcceptLanguage(lang); err == nil {
		for _, t := range tags {
			if cat, ok := catalogs[normalizeTag(t.String())]; ok {
				return cat
			}
			if base, conf := t.Base(); conf != language.No {
				if cat, ok := catalogs[normalizeTag(base.String())]; ok {
					return cat
				}
			}
		}
	}
	return nil
}

// normalizeTag folds the spellings a language tag is written in — gettext
// file names use `pt_BR`, BCP-47 and Accept-Language use `pt-BR`, case
// varies — to one map key. Same rule the format table applies
// (normalizeFormatLocale).
func normalizeTag(tag string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(tag), "_", "-"))
}

// LocaleFromContext returns the requested locale from ctx,
// reading the LAST value of `w17-language` gRPC metadata —
// the gateway's writers append least-specific first
// (auto-installed Accept-Language rename → authored
// `header_renames[]` → per-endpoint `metadata_bindings[]`),
// with the `default_language` stamp filling the key only when
// nothing above wrote it. Returns the configured default when
// no signal is present at all.
//
// Reads INCOMING metadata first, then OUTGOING: a backend reads
// the locale off what the transport delivered, while the gateway
// itself has no incoming gRPC metadata — the request locale it
// renders its own validation messages in lives on the outgoing
// side it is about to send.
func LocaleFromContext(ctx context.Context) string {
	// INCOMING first, OUTGOING second. A BACKEND reads the locale off
	// the metadata the transport delivered (incoming); a GATEWAY has
	// none — it is the one attaching metadata for the hop it is about
	// to make, so its own request locale lives on the OUTGOING side.
	//
	// Reading incoming only meant every message a gateway rendered
	// ITSELF — the whole generated request-validation block, which runs
	// before any backend call — fell straight through to the surface
	// default and ignored the caller's Accept-Language. Invisible until
	// a surface declared `default_language`: with the lib default the
	// fallback and the answer were the same string.
	for _, md := range []metadata.MD{incomingMD(ctx), outgoingMD(ctx)} {
		if vals := md.Get(LanguageMetadataKey); len(vals) > 0 {
			// LAST value wins. Every writer APPENDS, metadata preserves
			// the order, and the writers run least-specific first:
			//
			//	the auto-installed Accept-Language rename (weakest)
			//	author `header_renames[]` entries
			//	the per-endpoint `metadata_bindings[]` overlay (strongest)
			//	                 — emitted INSIDE the handler, so it can
			//	                   only ever append last
			//	the `default_language` stamp, which fires ONLY for a key
			//	no writer above populated (so it never competes)
			//
			// That last property is why the fallback cannot outrank a
			// caller's choice: MetadataDefaultStampMiddleware skips a
			// key that already has a value, and its
			// `metadata.FromOutgoingContext` check DOES observe pairs
			// added via `AppendToOutgoingContext` — grpc-go merges
			// `raw.added` into the returned MD (v1.82.1 metadata.go).
			//
			// Between 2026-07-31 and this fix the function read vals[0]
			// on the premise that that guard was blind, which it never
			// was. First-wins also made `metadata_bindings[]` inert on
			// any key another layer had written — the overlay appends
			// after every middleware by construction — and left this
			// reader disagreeing with FormatLocaleFromContext, restgw's
			// package doc and the comment the gateway emits into its own
			// handlers. One convention, and this is it.
			return vals[len(vals)-1]
		}
	}
	return DefaultLanguage()
}

// incomingMD / outgoingMD return the request metadata of each direction,
// nil when absent — so the caller can walk both without repeating the
// comma-ok dance.
func incomingMD(ctx context.Context) metadata.MD {
	md, _ := metadata.FromIncomingContext(ctx)
	return md
}

func outgoingMD(ctx context.Context) metadata.MD {
	md, _ := metadata.FromOutgoingContext(ctx)
	return md
}

// substitute walks `msg` once, left to right, replacing each `{name}`
// token with `params[name]`. Cheap early-out when the message has no `{`
// or params is empty. Unknown placeholder names pass through verbatim — a
// translation that referenced `{foo}` the caller didn't supply renders as
// literal `{foo}` rather than crashing.
//
// Single pass: a substituted value is copied to the output and never
// scanned again, so a parameter value that itself contains `{other}`
// (user input, typically) stays literal. The result depends only on
// `msg` and `params`, never on map iteration order.
func substitute(msg string, params map[string]string) string {
	if len(params) == 0 || !strings.ContainsRune(msg, '{') {
		return msg
	}
	var b strings.Builder
	b.Grow(len(msg))
	for i := 0; i < len(msg); {
		if msg[i] == '{' {
			if end := strings.IndexByte(msg[i+1:], '}'); end >= 0 {
				name := msg[i+1 : i+1+end]
				if v, ok := params[name]; ok && !strings.ContainsRune(name, '{') {
					b.WriteString(v)
					i += end + 2
					continue
				}
			}
		}
		b.WriteByte(msg[i])
		i++
	}
	return b.String()
}
