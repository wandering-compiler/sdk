package i18n

import (
	"context"
	"strings"

	"golang.org/x/text/language"
	"google.golang.org/grpc/metadata"
)

// FormatLocaleMetadataKey carries the per-request FORMAT locale — which is not
// the UI language. A Czech user running an English UI still wants
// `26.07.2026`: browsers separate the two (Accept-Language vs OS regional
// settings) and so does Django, so w17 does too
// (docs/specs/i18n/formatting.md §3).
//
// Lowercase ASCII per the gRPC convention, same last-write-wins precedence
// chain as [LanguageMetadataKey] (default stamp → header rename →
// metadata_bindings → cap call). Absent or empty → the request's
// `w17-language` is used, which is the right default: a locale is a better
// guess than a global constant.
const FormatLocaleMetadataKey = "w17-format-locale"

// FormatLocaleFromContext returns the locale VALUES should be formatted in:
// the last `w17-format-locale` metadata value, falling back to
// [LocaleFromContext] (i.e. `w17-language`, then the configured default).
//
// The result is a single tag, negotiated when necessary. The fallback source
// can be a RAW q-weighted Accept-Language list ("fr-FR,fr;q=0.9,cs;q=0.8"),
// because the gateway's Accept-Language → w17-language rename passes browser
// headers through verbatim — so a list is resolved to the highest-priority tag
// the frozen table actually carries, mirroring what negotiateCatalog does for
// message catalogs. A clean tag is returned untouched; [ResolveFormats] owns
// the base-language and `en` fallbacks from there.
func FormatLocaleFromContext(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(FormatLocaleMetadataKey); len(vals) > 0 {
			if v := strings.TrimSpace(vals[len(vals)-1]); v != "" {
				return negotiateFormatLocale(v)
			}
		}
	}
	return negotiateFormatLocale(LocaleFromContext(ctx))
}

// negotiateFormatLocale reduces a possibly-q-weighted Accept-Language list to
// one tag the frozen table knows. A value that is already a single tag — the
// overwhelmingly common case — is returned as-is after the cheap membership
// check fails, so an unknown-but-clean tag still reaches [ResolveFormats] and
// gets its base-language fallback there.
func negotiateFormatLocale(raw string) string {
	if raw == "" {
		return raw
	}
	if !strings.ContainsAny(raw, ",;") {
		return raw
	}
	tags, _, err := language.ParseAcceptLanguage(raw)
	if err != nil {
		return raw
	}
	for _, t := range tags {
		if HasFormatLocale(t.String()) {
			return t.String()
		}
		if base, conf := t.Base(); conf != language.No && HasFormatLocale(base.String()) {
			return base.String()
		}
	}
	// Nothing in the list is in the table: hand back the top preference and
	// let ResolveFormats fall through to `en`.
	if len(tags) > 0 {
		return tags[0].String()
	}
	return raw
}
