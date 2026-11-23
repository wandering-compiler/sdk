package i18n

import (
	"sort"
	"strconv"
	"strings"
)

// A merge never throws a translation away.
//
// When a source sentence changes, its msgid changes with it: the old entry is
// no longer harvested and the new one arrives untranslated. A merge that
// dropped the old entry took the translator's work with it — silently, since
// the rendered fallback (the English msgid) looks fine. Gettext's own answer,
// which this follows (msgmerge):
//
//   - an entry no longer harvested that carries a translation is kept at the
//     end of the catalog as an obsolete entry (`#~ msgid` / `#~ msgstr`),
//     which no runtime serves (gotext skips `#~` lines) and a translator can
//     still read;
//   - a new msgid close to an obsolete one takes its translation marked
//     `#, fuzzy`, with the old msgid in a `#| msgid` line: a translator
//     reviews the difference and removes the flag; until then the entry is
//     not served (the runtime warns about it);
//   - a msgid that comes back gets its obsolete translation back as it was.

// obsoleteEntry is one `#~` entry, or one being made obsolete.
type obsoleteEntry struct{ id, str string }

// fuzzyThreshold is the word-level similarity (1 − edit distance / longer
// length) a new msgid needs to an obsolete one to inherit its translation.
const fuzzyThreshold = 0.6

// parseObsolete reads the `#~` entries of body.
func parseObsolete(body []byte) []obsoleteEntry {
	var (
		out      []obsoleteEntry
		cur      *obsoleteEntry
		field    *string
		finished = func() {
			if cur != nil && cur.id != "" {
				out = append(out, *cur)
			}
			cur, field = nil, nil
		}
	)
	for _, raw := range strings.Split(string(body), "\n") {
		l := strings.TrimSpace(raw)
		rest, ok := strings.CutPrefix(l, "#~")
		if !ok {
			if l == "" || !strings.HasPrefix(l, "#") {
				finished()
			}
			continue
		}
		rest = strings.TrimSpace(rest)
		switch {
		case strings.HasPrefix(rest, "msgid "):
			finished()
			cur = &obsoleteEntry{}
			field = &cur.id
			*field = unquotePO(strings.TrimPrefix(rest, "msgid "))
		case strings.HasPrefix(rest, "msgstr ") && cur != nil:
			field = &cur.str
			*field = unquotePO(strings.TrimPrefix(rest, "msgstr "))
		case strings.HasPrefix(rest, `"`) && field != nil:
			*field += unquotePO(rest)
		}
	}
	finished()
	return out
}

func unquotePO(s string) string {
	u, err := strconv.Unquote(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return u
}

// quotePO writes s as one PO string literal.
func quotePO(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	return `"` + r.Replace(s) + `"`
}

// marshalObsolete appends the obsolete entries to body, sorted by msgid.
func marshalObsolete(body []byte, obsolete []obsoleteEntry) []byte {
	if len(obsolete) == 0 {
		return body
	}
	sort.Slice(obsolete, func(i, j int) bool { return obsolete[i].id < obsolete[j].id })
	var b strings.Builder
	b.Write(body)
	if !strings.HasSuffix(string(body), "\n\n") {
		if !strings.HasSuffix(string(body), "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	for i, o := range obsolete {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("#~ msgid " + quotePO(o.id) + "\n")
		b.WriteString("#~ msgstr " + quotePO(o.str) + "\n")
	}
	return []byte(b.String())
}

// similarity is 1 − (word edit distance / the longer length): 1 for the
// same words, 0 for nothing in common.
func similarity(a, b string) float64 {
	wa, wb := strings.Fields(a), strings.Fields(b)
	n, m := len(wa), len(wb)
	if n == 0 || m == 0 {
		return 0
	}
	if n > 2*m || m > 2*n {
		return 0
	}
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= n; i++ {
		cur[0] = i
		for j := 1; j <= m; j++ {
			cost := 1
			if wa[i-1] == wb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[m])/float64(max(n, m))
}

// closest is the obsolete entry most similar to id at or above
// fuzzyThreshold, by index; -1 when none is.
func closest(id string, obsolete []obsoleteEntry, used []bool) int {
	best, bestScore := -1, fuzzyThreshold
	for i, o := range obsolete {
		if used[i] {
			continue
		}
		if s := similarity(id, o.id); s >= bestScore && (best < 0 || s > bestScore) {
			best, bestScore = i, s
		}
	}
	return best
}
