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
// same context. Comment (`#…`) and blank lines are ignored.
func ValidatePO(body []byte) error {
	const (
		stNone   = iota // between entries
		stCtxt          // after msgctxt
		stID            // after msgid
		stPlural        // after msgid_plural
		stStr           // after msgstr / msgstr[N]
	)
	var (
		errs      []error
		state     = stNone
		ctxt, id  string
		entryLine int
		seen      = map[string]int{}
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
		state, ctxt, id = stNone, "", ""
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
			entryLine, state = n, stCtxt
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
				entryLine = n
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
	return errors.Join(errs...)
}
