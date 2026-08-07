package i18n

import (
	"embed"
	"encoding/json"
	"sort"
	"strings"
)

// formats.json is the FROZEN per-locale format table
// (docs/specs/i18n/formatting.md §1). It is reference data, not project
// configuration: a project cannot redefine what "Czech number formatting"
// means, the same way it cannot redefine what MAX_LEN_VIOLATION means. What a
// project can do is remap a (language, kind) pair onto another locale's row —
// see [ResolveFormatsKind].
//
// It is ALSO the source of truth for the TypeScript admin runtime, which
// carries a byte-identical mirror at sdk/ts/admin-runtime/src/formats.json
// (refreshed by `make sync-i18n-formats`, drift-gated by a test in srcgo). The
// two runtimes must format a value identically — that is the load-bearing
// property of this feature, and the shared golden vectors in
// testdata/format_vectors.json are what check it rather than hope for it.
//
//go:embed formats.json
var formatsFS embed.FS

// FormatKind is one axis of the override map: a project remaps formatting per
// KIND, not all-or-nothing ("Czech UI, English-looking numbers, Czech dates").
type FormatKind string

const (
	KindNumber   FormatKind = "number"
	KindDecimal  FormatKind = "decimal"
	KindPercent  FormatKind = "percent"
	KindDate     FormatKind = "date"
	KindTime     FormatKind = "time"
	KindDateTime FormatKind = "datetime"
)

// FormatKinds lists every valid override kind, in declaration order. The lock
// parser validates against this set.
func FormatKinds() []FormatKind {
	return []FormatKind{KindNumber, KindDecimal, KindPercent, KindDate, KindTime, KindDateTime}
}

// Formats is one locale's row of the frozen table.
//
// The pattern fields use a CLOSED token vocabulary rendered by
// [RenderTimePattern]: YYYY MM M DD D HH H hh h mm ss A. Nothing else is
// substituted, and because the table is frozen no project-authored pattern
// ever reaches it.
//
// FirstDayOfWeek is carried even though nothing formats with it: the admin's
// date pickers need it, it is the same class of data, so it is paid for once
// (0 = Sunday, 1 = Monday).
type Formats struct {
	DecimalSeparator  string `json:"decimal_separator"`
	ThousandSeparator string `json:"thousand_separator"`
	Grouping          int    `json:"grouping"`
	DateFormat        string `json:"date_format"`
	ShortDateFormat   string `json:"short_date_format"`
	TimeFormat        string `json:"time_format"`
	DateTimeFormat    string `json:"datetime_format"`
	PercentFormat     string `json:"percent_format"`
	FirstDayOfWeek    int    `json:"first_day_of_week"`
}

// FallbackFormatLocale is the last link of every resolution chain. A locale
// with no row of its own, and no base-language row, formats as `en`.
const FallbackFormatLocale = "en"

// Overrides is the project-owned remap that lives in the lock, keyed by
// language then kind: {"cs": {"number": "en"}} means "a cs request formats
// numbers the en way, everything else stays cs". Nil is the identity map.
type Overrides map[string]map[FormatKind]string

// formatTable is keyed by NORMALIZED locale (lowercase, `-` separated) so a
// tag written any of the ways it is written in the wild finds its row;
// formatSpelling keeps the canonical BCP-47 spelling from the file (`en-US`,
// not `en-us`) for anything a human reads, such as a parser's list of valid
// values.
var formatTable, formatSpelling = mustLoadFormatTable()

func mustLoadFormatTable() (map[string]Formats, map[string]string) {
	var doc struct {
		Schema  int                `json:"schema"`
		Locales map[string]Formats `json:"locales"`
	}
	body, err := formatsFS.ReadFile("formats.json")
	if err != nil {
		// Unreachable: the file is embedded at compile time, so a missing
		// file is a build failure, not a runtime one.
		panic("i18n: embedded formats.json unreadable: " + err.Error())
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		panic("i18n: embedded formats.json is not valid JSON: " + err.Error())
	}
	rows := make(map[string]Formats, len(doc.Locales))
	spelling := make(map[string]string, len(doc.Locales))
	for locale, row := range doc.Locales {
		key := normalizeFormatLocale(locale)
		if _, dup := rows[key]; dup {
			panic("i18n: formats.json declares " + locale + " twice (case/separator variants collide)")
		}
		rows[key], spelling[key] = row, locale
	}
	if _, ok := rows[FallbackFormatLocale]; !ok {
		panic("i18n: embedded formats.json has no " + FallbackFormatLocale + " row to fall back to")
	}
	return rows, spelling
}

// FormatTableJSON returns the frozen table exactly as it is embedded —
// `formats.json`'s own bytes, schema wrapper and all.
//
// It exists for CODE GENERATORS that have to put the table inside an artifact
// they emit (the generated web client bakes it into a TS module, since a
// dependency-free client cannot import a JSON file without the consumer's
// bundler agreeing to it). Baking the CANONICAL bytes rather than a mirror of
// them removes a hop where the two could disagree — the whole feature rests on
// every runtime formatting a value identically.
//
// The slice is a fresh copy; the embedded original stays immutable.
func FormatTableJSON() []byte {
	body, err := formatsFS.ReadFile("formats.json")
	if err != nil {
		// Unreachable for the same reason mustLoadFormatTable's panic is:
		// the file is embedded at compile time.
		panic("i18n: embedded formats.json unreadable: " + err.Error())
	}
	out := make([]byte, len(body))
	copy(out, body)
	return out
}

// FormatLocales lists every locale the frozen table carries in its canonical
// spelling, sorted. The lock parser validates an override VALUE against this
// set and prints it when rejecting one.
func FormatLocales() []string {
	out := make([]string, 0, len(formatSpelling))
	for _, canonical := range formatSpelling {
		out = append(out, canonical)
	}
	sort.Strings(out)
	return out
}

// HasFormatLocale reports whether the frozen table carries a row for exactly
// this locale — no fallback walking. Use it to validate a declaration;
// use [ResolveFormats] to format.
func HasFormatLocale(locale string) bool {
	_, ok := formatTable[normalizeFormatLocale(locale)]
	return ok
}

// ResolveFormats returns the row a locale formats with, walking the chain from
// docs/specs/i18n/formatting.md §2:
//
//  1. the locale itself (`cs` → the cs row, `pt-BR` → the pt-BR row if one exists)
//  2. its base language (`pt-BR` → `pt`)
//  3. `en`
//
// Case and separator are normalized first, so `CS`, `cs` and `cs_CZ` all land
// on the same row. Never fails: the en row is guaranteed present.
func ResolveFormats(locale string) Formats {
	l := normalizeFormatLocale(locale)
	if row, ok := formatTable[l]; ok {
		return row
	}
	if base, _, cut := strings.Cut(l, "-"); cut {
		if row, ok := formatTable[base]; ok {
			return row
		}
	}
	return formatTable[FallbackFormatLocale]
}

// ResolveFormatsKind is [ResolveFormats] with the project's override map
// applied first: a hit on (language, kind) replaces the locale before the
// chain runs. An override naming a locale with no row falls through to the
// ordinary chain rather than failing — the lock parser is what rejects a bad
// override, and a runtime must not break on data that got past it.
func ResolveFormatsKind(locale string, kind FormatKind, over Overrides) Formats {
	l := normalizeFormatLocale(locale)
	if byKind, ok := over[l]; ok {
		if target := normalizeFormatLocale(byKind[kind]); target != "" {
			if row, ok := formatTable[target]; ok {
				return row
			}
		}
	}
	return ResolveFormats(l)
}

// normalizeFormatLocale lowercases and switches `_` to `-` so a tag written
// any of the ways it is written in the wild resolves to one row. It does NOT
// parse Accept-Language: the format locale is a single tag by contract (see
// [FormatLocaleFromContext]), unlike `w17-language`, which can arrive as a raw
// q-weighted browser header.
func normalizeFormatLocale(locale string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(locale), "_", "-"))
}
