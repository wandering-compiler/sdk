package i18n

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ValidatePO checks a `.po` body against the gettext entry grammar and
// returns every problem found, each prefixed with its line number. A nil
// result means the file parses as written; an empty body is a valid, empty
// catalog.
//
// The runtime parser is lenient: it skips lines it does not understand and
// keeps whatever entries survive, so a truncated or hand-mangled catalog
// registers as a partial one without complaint. This check is what makes
// that visible — [RegisterCatalog] and [RegisterFS] run it on every catalog
// they register.
//
// Checked: keyword lines (`msgctxt`, `msgid`, `msgid_plural`, `msgstr`,
// `msgstr[N]`) carry exactly one well-formed quoted string; continuation
// strings follow a keyword; keywords appear in entry order (an entry's
// `msgstr` follows its `msgid`, indexed forms only after `msgid_plural`);
// every entry ends with a translation; no msgid is defined twice in the
// same context; no entry is marked `#, fuzzy` (a translator's unreviewed
// guess, which the runtime does not serve — see [RegisterCatalog]; the
// header entry is exempt). Other comment (`#…`) and blank lines are ignored.
func ValidatePO(body []byte) error {
	errs, entries := scanPO(body)
	for _, e := range entries {
		if e.fuzzy && e.id != "" {
			errs = append(errs, fmt.Errorf("line %d: entry %q is marked fuzzy and is not served until the flag is removed", e.line, e.id))
		}
	}
	return errors.Join(errs...)
}

// poEntryPos is one entry [scanPO] found: its context and msgid (continuation
// strings joined), the line its first keyword is on, and whether a `#,`
// flag line before it carries `fuzzy`.
type poEntryPos struct {
	ctxt, id string
	line     int
	fuzzy    bool
}

// fuzzyIDs is the set of msgids (with no msgctxt) of body's fuzzy entries,
// the header excepted.
func fuzzyIDs(body []byte) map[string]bool {
	_, entries := scanPO(body)
	var out map[string]bool
	for _, e := range entries {
		if e.fuzzy && e.id != "" && e.ctxt == "" {
			if out == nil {
				out = map[string]bool{}
			}
			out[e.id] = true
		}
	}
	return out
}

// isFuzzyFlagLine reports whether l is a `#,` flag line naming `fuzzy`.
func isFuzzyFlagLine(l string) bool {
	rest, ok := strings.CutPrefix(l, "#,")
	if !ok {
		return false
	}
	for _, f := range strings.Split(rest, ",") {
		if strings.TrimSpace(f) == "fuzzy" {
			return true
		}
	}
	return false
}

// scanPO walks body by the gettext entry grammar ValidatePO documents and
// returns the grammar problems and the entries found.
func scanPO(body []byte) ([]error, []poEntryPos) {
	const (
		stNone   = iota // between entries
		stCtxt          // after msgctxt
		stID            // after msgid
		stPlural        // after msgid_plural
		stStr           // after msgstr / msgstr[N]
	)
	var (
		errs                     []error
		entries                  []poEntryPos
		state                    = stNone
		ctxt, id                 string
		entryLine                int
		seen                     = map[string]int{}
		pendingFuzzy, entryFuzzy bool
	)
	fail := func(line int, format string, args ...any) {
		errs = append(errs, fmt.Errorf("line %d: "+format, append([]any{line}, args...)...))
	}
	finish := func() {
		if state == stNone {
			return
		}
		if state != stStr {
			fail(entryLine, "entry %q has no msgstr", id)
		}
		key := ctxt + "\x04" + id
		if first, dup := seen[key]; dup {
			fail(entryLine, "msgid %q already defined at line %d", id, first)
		} else {
			seen[key] = entryLine
		}
		entries = append(entries, poEntryPos{ctxt: ctxt, id: id, line: entryLine, fuzzy: entryFuzzy})
		state, ctxt, id, entryFuzzy = stNone, "", "", false
	}
	// open starts an entry at line n, taking the flags read since the last one.
	open := func(n int) {
		entryLine, entryFuzzy, pendingFuzzy = n, pendingFuzzy, false
	}
	// quoted parses the one quoted string a keyword line carries.
	quoted := func(line int, rest string) (string, bool) {
		rest = strings.TrimSpace(rest)
		if len(rest) < 2 || rest[0] != '"' || rest[len(rest)-1] != '"' {
			fail(line, "expected a quoted string, got %q", rest)
			return "", false
		}
		s, err := strconv.Unquote(rest)
		if err != nil {
			fail(line, "malformed quoted string %s", rest)
			return "", false
		}
		return s, true
	}

	for i, raw := range strings.Split(string(body), "\n") {
		n := i + 1
		l := strings.TrimSpace(raw)
		switch {
		case l == "" || strings.HasPrefix(l, "#"):
			if isFuzzyFlagLine(l) {
				pendingFuzzy = true
			}
			continue

		case strings.HasPrefix(l, `"`):
			if state == stNone {
				fail(n, "string continuation outside an entry")
				continue
			}
			s, ok := quoted(n, l)
			if !ok {
				continue
			}
			switch state {
			case stID:
				id += s
			case stCtxt:
				ctxt += s
			}

		case strings.HasPrefix(l, "msgctxt"):
			if state == stStr {
				finish()
			} else if state != stNone {
				fail(n, "msgctxt inside an unfinished entry")
				finish()
			}
			open(n)
			state = stCtxt
			ctxt, _ = quoted(n, strings.TrimPrefix(l, "msgctxt"))

		case strings.HasPrefix(l, "msgid_plural"):
			if state != stID {
				fail(n, "msgid_plural without a preceding msgid")
				continue
			}
			state = stPlural
			quoted(n, strings.TrimPrefix(l, "msgid_plural"))

		case strings.HasPrefix(l, "msgid"):
			switch state {
			case stStr:
				finish()
			case stCtxt:
				// msgctxt opens the entry; keep its line and context.
			case stNone:
			default:
				fail(n, "msgid inside an unfinished entry")
				finish()
			}
			if state == stNone {
				open(n)
			}
			state = stID
			id, _ = quoted(n, strings.TrimPrefix(l, "msgid"))

		case strings.HasPrefix(l, "msgstr["):
			end := strings.IndexByte(l, ']')
			if end < 0 {
				fail(n, "unterminated msgstr index")
				continue
			}
			if _, err := strconv.Atoi(l[len("msgstr["):end]); err != nil {
				fail(n, "msgstr index %q is not a number", l[len("msgstr["):end])
				continue
			}
			if state != stPlural && state != stStr {
				fail(n, "indexed msgstr without msgid_plural")
				continue
			}
			state = stStr
			quoted(n, l[end+1:])

		case strings.HasPrefix(l, "msgstr"):
			switch state {
			case stID:
			case stPlural:
				fail(n, "plural entry needs indexed msgstr[N]")
			default:
				fail(n, "msgstr without a preceding msgid")
				continue
			}
			state = stStr
			quoted(n, strings.TrimPrefix(l, "msgstr"))

		default:
			fail(n, "unrecognised line %q", l)
		}
	}
	finish()
	return errs, entries
}
