package i18n

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/leonelquinteros/gotext"
)

// POEntry is one msgid → msgstr pair plus optional translator
// metadata. Used by [MarshalPO] to assemble a `.po` catalog
// body. Comments / references are surfaced as `#.` / `#:`
// lines in the output per gettext convention.
type POEntry struct {
	// Msgid is the canonical source string (English template
	// with `{placeholder}` tokens preserved). Acts as the
	// catalog lookup key at runtime.
	Msgid string

	// Msgstr is the translation. Empty for scaffolded
	// language files — translator fills in; runtime falls
	// back to msgid via the default-locale chain.
	Msgstr string

	// ExtractedComment is the translator-facing context line
	// emitted as `#. <text>` above the entry. Use it for
	// "Source: validation type X" hints. Empty = no comment.
	ExtractedComment string

	// Reference is the source-code reference line emitted as
	// `#: <text>`. Use it for "file:line where this msgid
	// originated". Empty = no reference.
	Reference string

	// Fuzzy marks the entry `#, fuzzy`: a translation not yet
	// reviewed, which the runtime does not serve. MarshalPO writes
	// the flag; MergePO keeps it; EntriesFromPO reports it (with an
	// empty Msgstr, so a catalog built from it does not serve it).
	Fuzzy bool
}

// MarshalPO serialises the supplied entries into a gettext
// `.po` file body keyed under `lang`. Output uses gotext's
// canonical header order (Project-Id-Version, Language, MIME
// headers, …) so two runs producing the same entry set
// produce byte-identical files (modulo POT-Creation-Date,
// which advances on every call; a caller that needs byte-identical
// output treats that one header line as noise).
//
// Entries are sorted by msgid before emit; ordering is stable
// across runs given the same input.
//
// `lang` is the BCP-47 tag landing in the `Language:` header
// + filename convention. Empty `lang` is a parse-time error
// — gettext consumers don't tolerate a missing Language header.
func MarshalPO(lang string, entries []POEntry) ([]byte, error) {
	return marshalPO(lang, entries, nil, nil)
}

// ownHeaders are the header fields MarshalPO writes itself; a prior
// catalog's other fields are carried through a merge.
var ownHeaders = map[string]bool{
	"project-id-version": true, "pot-creation-date": true, "language": true,
	"mime-version": true, "content-type": true, "content-transfer-encoding": true,
}

// marshalPO is MarshalPO plus what a merge carries over from the catalog it
// replaces: header fields MarshalPO does not write itself (keep), and the
// translator comments (`# …`) of each msgid (comments).
func marshalPO(lang string, entries []POEntry, keep gotext.HeaderMap, comments map[string][]string) ([]byte, error) {
	if lang == "" {
		return nil, fmt.Errorf("i18n: MarshalPO: lang is empty")
	}
	po := gotext.NewPo()
	d := po.GetDomain()
	if d == nil {
		return nil, fmt.Errorf("i18n: MarshalPO: gotext returned a nil Domain")
	}
	d.Headers = gotext.HeaderMap{}
	d.Headers.Set("Project-Id-Version", "w17")
	d.Headers.Set("POT-Creation-Date", time.Now().UTC().Format("2006-01-02 15:04-0700"))
	d.Headers.Set("Language", lang)
	d.Headers.Set("MIME-Version", "1.0")
	d.Headers.Set("Content-Type", "text/plain; charset=UTF-8")
	d.Headers.Set("Content-Transfer-Encoding", "8bit")
	for k, vs := range keep {
		if !ownHeaders[strings.ToLower(k)] && len(vs) > 0 {
			d.Headers[k] = append([]string(nil), vs...)
		}
	}

	sorted := make([]POEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Msgid < sorted[j].Msgid
	})

	for _, e := range sorted {
		if e.Msgid == "" {
			// Empty msgid is the header slot; we control that
			// via Headers above. Skip any user-supplied empty
			// msgid so callers don't accidentally clobber it.
			continue
		}
		d.Set(e.Msgid, e.Msgstr)
		var refs []string
		if e.Reference != "" {
			refs = append(refs, e.Reference)
		}
		if len(refs) > 0 {
			d.SetRefs(e.Msgid, refs)
		}
		// ExtractedComment isn't surfaced by Domain's public
		// API in this gotext version; the `#.` line would
		// require touching Translation internals. Left as a
		// follow-up — translators get just the Reference for
		// now, which is the more load-bearing field.
		_ = e.ExtractedComment
	}

	out, err := d.MarshalText()
	if err != nil {
		return nil, err
	}
	return addTranslatorComments(markFuzzy(out, sorted), comments), nil
}

// addTranslatorComments inserts each msgid's translator comments above its
// entry, before the other comment lines (`#:`, `#,`), as gettext orders them.
func addTranslatorComments(body []byte, comments map[string][]string) []byte {
	if len(comments) == 0 {
		return body
	}
	lines := strings.SplitAfter(string(body), "\n")
	_, found := scanPO(body)
	at := map[int][]string{} // 0-based line index → comments to insert before it
	for _, e := range found {
		c := comments[e.id]
		if e.ctxt != "" || e.id == "" || len(c) == 0 {
			continue
		}
		start := e.line - 1
		for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
			start--
		}
		at[start] = c
	}
	var b strings.Builder
	for i, l := range lines {
		for _, c := range at[i] {
			b.WriteString(c)
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return []byte(b.String())
}

// translatorComments reads the translator comments (`#` alone or `# …`;
// not `#.`, `#:`, `#,`, `#|`, `#~`) above each entry of body, by msgid.
func translatorComments(body []byte) map[string][]string {
	lines := strings.Split(string(body), "\n")
	_, found := scanPO(body)
	var out map[string][]string
	for _, e := range found {
		if e.ctxt != "" || e.id == "" {
			continue
		}
		var c []string
		for i := e.line - 2; i >= 0; i-- {
			l := strings.TrimSpace(lines[i])
			if !strings.HasPrefix(l, "#") {
				break
			}
			if l == "#" || strings.HasPrefix(l, "# ") {
				c = append([]string{l}, c...)
			}
		}
		if len(c) > 0 {
			if out == nil {
				out = map[string][]string{}
			}
			out[e.id] = c
		}
	}
	return out
}

// markFuzzy inserts a `#, fuzzy` line before each entry of body that
// entries marks Fuzzy (the gotext writer has no flag lines of its own).
func markFuzzy(body []byte, entries []POEntry) []byte {
	want := map[string]bool{}
	for _, e := range entries {
		if e.Fuzzy && e.Msgid != "" {
			want[e.Msgid] = true
		}
	}
	if len(want) == 0 {
		return body
	}
	_, found := scanPO(body)
	at := map[int]bool{}
	for _, e := range found {
		if e.ctxt == "" && want[e.id] {
			at[e.line] = true
		}
	}
	lines := strings.SplitAfter(string(body), "\n")
	var b strings.Builder
	b.Grow(len(body) + 10*len(at))
	for i, l := range lines {
		if at[i+1] {
			b.WriteString("#, fuzzy\n")
		}
		b.WriteString(l)
	}
	return []byte(b.String())
}

// ScaffoldPO returns a `.po` body for `lang` containing every
// supplied msgid with EMPTY msgstr — the shape a translator
// receives when a new language is added to the project.
//
// Convenience for `MarshalPO(lang, [{Msgid: m, Msgstr: ""}…])`.
func ScaffoldPO(lang string, msgids []string) ([]byte, error) {
	entries := make([]POEntry, 0, len(msgids))
	for _, m := range msgids {
		entries = append(entries, POEntry{Msgid: m})
	}
	return MarshalPO(lang, entries)
}

// DefaultLocalePO returns a `.po` body for `lang` where each
// supplied msgid is also its own msgstr — the EN baseline
// every project ships. Runtime falls through to msgid even
// without this file, but emitting it makes the catalog
// inspectable + provides a target for english copy-editing
// without code changes.
func DefaultLocalePO(lang string, msgids []string) ([]byte, error) {
	entries := make([]POEntry, 0, len(msgids))
	for _, m := range msgids {
		entries = append(entries, POEntry{Msgid: m, Msgstr: m})
	}
	return MarshalPO(lang, entries)
}

// MarshalJSON serialises the supplied entries into a compact
// `{msgid: msgstr, ...}` JSON document. This is the catalog
// shape FE clients (admin SPA + generated TS client) consume —
// strictly smaller than the `.po` form and parseable with
// `JSON.parse` alone, no gettext dependency on the client.
//
// Empty msgstrs are preserved as empty strings; the client-side
// T helper falls back to the bare msgid when msgstr is empty,
// matching the server-side gotext fallback chain.
//
// Output is sorted by msgid + uses a stable 2-space indent for
// human-readable diffs in commits.
func MarshalJSON(lang string, entries []POEntry) ([]byte, error) {
	if lang == "" {
		return nil, fmt.Errorf("i18n: MarshalJSON: lang is empty")
	}
	sorted := make([]POEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Msgid < sorted[j].Msgid
	})
	// json.Marshal on a map randomises iteration order; build
	// a json.RawMessage tree by hand so the on-disk file diffs
	// cleanly across regens.
	var b []byte
	b = append(b, '{', '\n')
	for i, e := range sorted {
		if e.Msgid == "" {
			continue
		}
		key, err := json.Marshal(e.Msgid)
		if err != nil {
			return nil, fmt.Errorf("i18n: MarshalJSON: msgid: %w", err)
		}
		val, err := json.Marshal(e.Msgstr)
		if err != nil {
			return nil, fmt.Errorf("i18n: MarshalJSON: msgstr: %w", err)
		}
		b = append(b, ' ', ' ')
		b = append(b, key...)
		b = append(b, ':', ' ')
		b = append(b, val...)
		if i < len(sorted)-1 {
			b = append(b, ',')
		}
		b = append(b, '\n')
	}
	b = append(b, '}', '\n')
	return b, nil
}

// MergePO merges `msgids` (the up-to-date harvested set) into
// `existing` (the on-disk `.po` body that may carry translator-
// authored msgstrs). Returns a fresh `.po` body where:
//
//   - every msgid in the union appears,
//   - msgstrs from `existing` are preserved verbatim,
//   - new msgids (in `msgids` but not in `existing`) carry the
//     EN baseline (msgstr=msgid) when `lang ==
//     CompileTimeDefaultLanguage` and empty msgstr otherwise,
//   - obsolete entries (in `existing` but not in `msgids`)
//     drop silently, which keeps the format tight; gettext's
//     `#~ msgid` comment-out is not emitted,
//   - an entry's `#, fuzzy` flag and its translator comments
//     (`# …`) are kept, and so are the header fields MarshalPO
//     does not write itself (Plural-Forms, Last-Translator,
//     Language-Team, PO-Revision-Date, …).
//
// Use this from codegen orchestrators that want regen to keep
// adding new msgids over time without overwriting translations.
// First-time emit (no file on disk yet) goes through
// [EmitDomainCatalogs]; once the file exists, every subsequent
// regen funnels through MergePO.
func MergePO(existing []byte, lang string, msgids []string) ([]byte, error) {
	if lang == "" {
		return nil, fmt.Errorf("i18n: MergePO: lang is empty")
	}
	prior := gotext.NewPo()
	prior.Parse(existing)
	priorFuzzy := fuzzyIDs(existing)
	priorMsgstr := map[string]string{}
	var keep gotext.HeaderMap
	if d := prior.GetDomain(); d != nil {
		keep = d.Headers
		for _, t := range d.GetTranslations() {
			if t == nil || t.ID == "" {
				continue
			}
			priorMsgstr[t.ID] = rawMsgstr(t)
		}
	}
	entries := make([]POEntry, 0, len(msgids))
	for _, m := range msgids {
		msgstr, ok := priorMsgstr[m]
		if !ok && lang == CompileTimeDefaultLanguage {
			msgstr = m
		}
		entries = append(entries, POEntry{Msgid: m, Msgstr: msgstr, Fuzzy: ok && priorFuzzy[m]})
	}
	return marshalPO(lang, entries, keep, translatorComments(existing))
}

// EntriesFromMsgids returns the POEntry slice that mirrors what
// [DefaultLocalePO] / [ScaffoldPO] emit — EN baseline (msgstr=
// msgid) for the compile-time default language, empty msgstrs
// otherwise. Used by the JSON catalog emit path to share the
// fresh-seed shape with the `.po` emit path.
func EntriesFromMsgids(lang string, msgids []string) []POEntry {
	out := make([]POEntry, 0, len(msgids))
	for _, m := range msgids {
		entry := POEntry{Msgid: m}
		if lang == CompileTimeDefaultLanguage {
			entry.Msgstr = m
		}
		out = append(out, entry)
	}
	return out
}

// EntriesFromPO parses `body` (a `.po` body) into POEntry
// records. Used by the JSON emitter so the JSON catalog
// mirrors whatever the merged `.po` ended up with — including
// translator-authored msgstrs.
func EntriesFromPO(body []byte) []POEntry {
	po := gotext.NewPo()
	po.Parse(body)
	d := po.GetDomain()
	if d == nil {
		return nil
	}
	trans := d.GetTranslations()
	fuzzy := fuzzyIDs(body)
	out := make([]POEntry, 0, len(trans))
	for _, t := range trans {
		if t == nil || t.ID == "" {
			continue
		}
		if fuzzy[t.ID] {
			out = append(out, POEntry{Msgid: t.ID, Fuzzy: true})
			continue
		}
		out = append(out, POEntry{Msgid: t.ID, Msgstr: rawMsgstr(t)})
	}
	return out
}

// rawMsgstr reads a parsed translation's msgstr WITHOUT gotext's
// untranslated fallback.
//
// `Translation.Get()` returns the msgid when the msgstr is empty
// — right for a runtime lookup, wrong for anything that writes a
// catalog back out. Through Get, every UNTRANSLATED entry came
// back "translated" to its own English source, so each merge
// silently marked the whole catalog done: a translator lost the
// one signal that says what is left, and the drift was invisible
// because the rendered text is identical either way.
func rawMsgstr(t *gotext.Translation) string {
	if t == nil {
		return ""
	}
	// Index 0 is the singular form; gotext stores plurals at 1..n and
	// nothing here reads them (POEntry carries one msgstr).
	return t.Trs[0]
}

// DomainCatalogFile is one `.po` ready for the codegen
// orchestrator to write at a project-relative path. The
// orchestrator interprets [Path] relative to the project
// root + applies the "if not exists" semantic so translator
// edits survive regens.
type DomainCatalogFile struct {
	// Path is project-relative (e.g.
	// "w17/languages/app/cs.po"). The orchestrator joins
	// this with the project root before writing.
	Path string

	// Body is the .po file body bytes — EN baseline
	// (msgstr=msgid) for the CompileTimeDefaultLanguage,
	// scaffolded (empty msgstr) for every other language.
	Body []byte

	// Lang is the BCP-47 tag this entry targets; kept for
	// logging / diag rather than written into the file
	// (Marshal stamps it into the `Language:` header).
	Lang string
}

// EmitDomainCatalogs assembles every `.po` body the project's
// codegen seeds for one domain. One entry per
// language in `languages`; the EN entry carries the baseline
// (msgstr=msgid), every other language is scaffolded.
//
// `domain` is the proto-package last segment (e.g. "app",
// "billing") — the same value generated bundles use as their
// domain identifier. `languagesDir` is the project-relative
// catalog root (typically `lock.EffectiveLanguagesDir(lk)`).
//
// `msgids` is the deduplicated msgid set the catalog covers —
// today it is `validation.Defaults()`; per-domain author
// overrides and the ACL / cursor / parse vocabulary may be
// merged in later.
//
// Empty `languages` collapses to `[CompileTimeDefaultLanguage]`
// (the baseline-only catalog every project carries).
//
// **Output writes are "if not exists" at the orchestrator
// level** — translator edits to existing `.po` files
// MUST survive regen. The codegen seeds a missing file here;
// once it exists, a regen adds new msgids through [MergePO],
// which keeps the translations.
func EmitDomainCatalogs(domain, languagesDir string, languages, msgids []string) ([]DomainCatalogFile, error) {
	if domain == "" {
		return nil, fmt.Errorf("i18n: EmitDomainCatalogs: domain is empty")
	}
	if languagesDir == "" {
		return nil, fmt.Errorf("i18n: EmitDomainCatalogs: languagesDir is empty")
	}
	if len(languages) == 0 {
		languages = []string{CompileTimeDefaultLanguage}
	}
	out := make([]DomainCatalogFile, 0, len(languages))
	for _, lang := range languages {
		var body []byte
		var err error
		// Convention: EN is always the canonical baseline
		// (msgstr mirrors msgid); every other language is
		// scaffolded with empty msgstr for translators.
		if lang == CompileTimeDefaultLanguage {
			body, err = DefaultLocalePO(lang, msgids)
		} else {
			body, err = ScaffoldPO(lang, msgids)
		}
		if err != nil {
			return nil, fmt.Errorf("i18n: domain %s lang %s: %w", domain, lang, err)
		}
		out = append(out, DomainCatalogFile{
			Path: languagesDir + "/" + domain + "/" + lang + ".po",
			Body: body,
			Lang: lang,
		})
	}
	return out, nil
}
