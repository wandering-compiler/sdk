package migrate

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// CheckTimeout bounds one pre-apply check: the appliers set it as the
// statement timeout (Postgres statement_timeout, MySQL max_execution_time)
// and as the context deadline.
const CheckTimeout = 30 * time.Second

// ErrCheckRefused marks a check whose SQL is not one the planner renders.
var ErrCheckRefused = errors.New("pre-apply check refused")

// CheckDialect selects how ValidateCheckSQL tokenizes a check: the comment,
// string and identifier rules of the server that will run it.
type CheckDialect int

const (
	// CheckPostgres: `--` to end of line and NESTED /* */ comments, no `#`
	// comment; '…' strings with '' doubling and no backslash escape
	// (standard_conforming_strings, which the applier's session pins); "…"
	// identifiers.
	CheckPostgres CheckDialect = iota
	// CheckMySQL: `#` and `-- ` (dash dash and whitespace) comments, /* */
	// not nested and /*! */ /*+ */ refused; '…' and "…" strings with
	// backslash escapes (the default sql_mode); `…` identifiers.
	CheckMySQL
	// CheckSQLite: `--` and non-nested /* */ comments; '…' strings without
	// backslash escapes; "…" and `…` identifiers.
	CheckSQLite
)

// checkWords are the bare words a check may use outside a function call:
// the keywords, type names and aliases of the planner's probe templates
// (classifier CheckSQL cells and the restart guards) and nothing else. A
// test in the platform renders every template and requires each to pass.
var checkWords = wordSet(
	"select", "from", "where", "and", "or", "not", "is", "null", "in", "exists", "between",
	"limit", "group", "by", "having", "distinct", "as", "case", "when", "then", "else", "end",
	"true", "false", "with", "recursive", "union", "all", "cross", "join", "lateral",
	// type names the probes cast to
	"numeric", "double", "precision", "real", "float8", "text", "time", "inet", "json",
	// the aliases the templates name
	"src", "nodes", "n", "c", "k", "v", "child", "value",
)

// checkFuncs are the functions a check may call: those the probe templates
// call. Any identifier — bare or quoted — followed by `(` is a call; a quoted
// one never is a template's.
var checkFuncs = wordSet(
	"count", "abs", "floor", "char_length", "scale", "host", "network",
	"regexp_replace", "json_each", "json_array_elements", "json_object_keys", "json_typeof",
)

// CheckAllowlist returns the words and functions ValidateCheckSQL accepts, so
// the planner's side can prove every entry is one its templates use.
func CheckAllowlist() (words, funcs []string) {
	for w := range checkWords {
		words = append(words, w)
	}
	for f := range checkFuncs {
		funcs = append(funcs, f)
	}
	return words, funcs
}

// checkPunct is the punctuation a check may use.
const checkPunct = "(),.*+-/<>=!~:^%|"

func wordSet(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

type checkTokKind int

const (
	tokWord checkTokKind = iota
	tokIdent
	tokString
	tokNumber
	tokPunct
)

type checkTok struct {
	kind checkTokKind
	text string
}

// ValidateCheckSQL accepts only what the planner renders: ONE statement, a
// SELECT, built from the probe templates' keywords and functions (an
// allowlist), tokenized the way d's server reads it — so a quote, a comment
// or a quoted function name cannot hide a second statement or a call the
// templates never make.
//
// The checks are bound into the migration's content hash, so they are as
// trusted as its body; this is defence in depth for a check that reaches the
// applier anyway.
func ValidateCheckSQL(sql string, d CheckDialect) error {
	return validateCheck(sql, d, nil)
}

// ValidateMigrationCheckSQL is ValidateCheckSQL for a check of the migration
// whose up body is `body`: the check may also repeat, token for token, a
// parenthesized group or a call `f(…)` that the body contains outside its
// strings and comments. A unique index over `lower(email)` has a uniqueness
// probe over that same expression, and an index expression is the author's
// DQL — its functions, its bare column names, its casts — which no fixed list
// can name. The body evaluates that text anyway, over every row, with more
// power than a read-only probe, and both are bound into the same content
// hash. Nothing else widens: outside such a span every rule of
// ValidateCheckSQL stands, and a `;` is refused even inside one.
func ValidateMigrationCheckSQL(sql, body string, d CheckDialect) error {
	if strings.TrimSpace(body) == "" {
		return validateCheck(sql, d, nil)
	}
	b, _ := scanCheck(body, d, true)
	return validateCheck(sql, d, b)
}

func validateCheck(sql string, d CheckDialect, body []checkTok) error {
	toks, err := tokenizeCheck(sql, d)
	if err != nil {
		return err
	}
	if n := len(toks); n > 0 && toks[n-1].kind == tokPunct && toks[n-1].text == ";" {
		toks = toks[:n-1]
	}
	if len(toks) == 0 || toks[0].kind != tokWord || strings.ToLower(toks[0].text) != "select" {
		return fmt.Errorf("%w: not a SELECT", ErrCheckRefused)
	}
	depth := 0
	for _, t := range toks {
		if t.kind != tokPunct {
			continue
		}
		switch t.text {
		case ";":
			return fmt.Errorf("%w: more than one statement", ErrCheckRefused)
		case "(":
			depth++
		case ")":
			if depth--; depth < 0 {
				return fmt.Errorf("%w: unbalanced parentheses", ErrCheckRefused)
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("%w: unbalanced parentheses", ErrCheckRefused)
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		call := i+1 < len(toks) && isOpen(toks[i+1])
		// A span the body holds verbatim: a call through a bare name with its
		// argument list, or a parenthesized group.
		if body != nil && (t.kind == tokWord && call || isOpen(t)) {
			start := i
			if t.kind == tokWord {
				start = i + 1
			}
			if end := closeOf(toks, start); end > 0 && containsSeq(body, toks[i:end+1]) {
				i = end
				continue
			}
		}
		switch t.kind {
		case tokIdent:
			if call {
				return fmt.Errorf("%w: a call through a quoted name (%s)", ErrCheckRefused, t.text)
			}
		case tokWord:
			w := strings.ToLower(t.text)
			if call {
				if !checkFuncs[w] && !checkWords[w] {
					return fmt.Errorf("%w: it calls %s(), which no probe template calls and the migration's body does not contain", ErrCheckRefused, w)
				}
				continue
			}
			if !checkWords[w] && !checkFuncs[w] {
				return fmt.Errorf("%w: %q is not part of a probe", ErrCheckRefused, strings.ToUpper(w))
			}
		}
	}
	return nil
}

func isOpen(t checkTok) bool { return t.kind == tokPunct && t.text == "(" }

// closeOf returns the index of the `)` closing the `(` at toks[open], or -1.
func closeOf(toks []checkTok, open int) int {
	depth := 0
	for j := open; j < len(toks); j++ {
		if toks[j].kind != tokPunct {
			continue
		}
		switch toks[j].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// containsSeq reports whether seq occurs contiguously in body, a word
// matching case-insensitively and every other token exactly.
func containsSeq(body, seq []checkTok) bool {
	for i := 0; i+len(seq) <= len(body); i++ {
		k := 0
		for ; k < len(seq); k++ {
			a, b := body[i+k], seq[k]
			if a.kind != b.kind || (a.text != b.text && !(a.kind == tokWord && strings.EqualFold(a.text, b.text))) {
				break
			}
		}
		if k == len(seq) {
			return true
		}
	}
	return false
}

func tokenizeCheck(sql string, d CheckDialect) ([]checkTok, error) {
	return scanCheck(sql, d, false)
}

// scanCheck tokenizes s the way d's server reads it. Strict (a check), it
// refuses whatever no probe template contains; lenient (a migration body, see
// ValidateMigrationCheckSQL), it reads past those instead and stops at an unterminated quote
// or comment.
func scanCheck(s string, d CheckDialect, lenient bool) ([]checkTok, error) {
	var out []checkTok
	refuse := func(what string) ([]checkTok, error) {
		if lenient {
			return out, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrCheckRefused, what)
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		// ---- (lenient only) what a body has and a check never does
		case lenient && c == '$' && d == CheckPostgres:
			// A dollar-quoted string — a routine's body among them: what it
			// calls runs when the routine does, not when the migration does.
			j := i + 1
			for j < len(s) && isCheckWordByte(s[j]) {
				j++
			}
			if j >= len(s) || s[j] != '$' {
				i = j
				continue
			}
			tag := s[i : j+1]
			end := strings.Index(s[j+1:], tag)
			if end < 0 {
				return out, nil
			}
			out = append(out, checkTok{tokString, s[i : j+1+end+len(tag)]})
			i = j + 1 + end + len(tag)
		case lenient && d == CheckPostgres && (c == 'e' || c == 'E') && i+1 < len(s) && s[i+1] == '\'' &&
			(i == 0 || !isCheckWordByte(s[i-1])):
			// E'…': backslash escapes.
			j := i + 2
			for ; j < len(s); j++ {
				if s[j] == '\\' {
					j++
					continue
				}
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						j++
						continue
					}
					break
				}
			}
			if j >= len(s) {
				return out, nil
			}
			out = append(out, checkTok{tokString, s[i : j+1]})
			i = j + 1
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		// ---- comments
		case c == '-' && i+1 < len(s) && s[i+1] == '-' &&
			(d != CheckMySQL || i+2 >= len(s) || s[i+2] <= ' '):
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '#' && d == CheckMySQL:
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			if d == CheckMySQL && i+2 < len(s) && (s[i+2] == '!' || s[i+2] == '+') {
				if lenient {
					// its content is code the server runs: read on
					i += 3
					continue
				}
				return refuse("an executable comment")
			}
			if d == CheckPostgres {
				depth, j := 1, i+2
				for j < len(s) && depth > 0 {
					switch {
					case s[j] == '/' && j+1 < len(s) && s[j+1] == '*':
						depth++
						j += 2
					case s[j] == '*' && j+1 < len(s) && s[j+1] == '/':
						depth--
						j += 2
					default:
						j++
					}
				}
				if depth > 0 {
					return refuse("an unterminated comment")
				}
				i = j
				continue
			}
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return refuse("an unterminated comment")
			}
			i += end + 4
		// ---- strings and quoted identifiers
		case c == '\'' || c == '"' || c == '`':
			kind := tokIdent
			if c == '\'' || (c == '"' && d == CheckMySQL) {
				kind = tokString
			}
			if c == '`' && d == CheckPostgres {
				if lenient {
					i++
					continue
				}
				return refuse("a backtick")
			}
			backslash := kind == tokString && d == CheckMySQL
			j := i + 1
			for ; j < len(s); j++ {
				if backslash && s[j] == '\\' {
					j++
					continue
				}
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c {
						j++
						continue
					}
					break
				}
			}
			if j >= len(s) {
				return refuse("an unterminated quote")
			}
			out = append(out, checkTok{kind, s[i : j+1]})
			i = j + 1
		// ---- numbers
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
				k := j + 1
				if k < len(s) && (s[k] == '+' || s[k] == '-') {
					k++
				}
				if k < len(s) && s[k] >= '0' && s[k] <= '9' {
					for k < len(s) && s[k] >= '0' && s[k] <= '9' {
						k++
					}
					j = k
				}
			}
			if j < len(s) && isCheckWordByte(s[j]) && !lenient {
				return refuse("a number running into a word")
			}
			out = append(out, checkTok{tokNumber, s[i:j]})
			i = j
		// ---- words
		case isCheckWordStart(c):
			j := i
			for j < len(s) && isCheckWordByte(s[j]) {
				j++
			}
			if j < len(s) && !lenient && (s[j] == '\'' || s[j] == '"' || s[j] == '`' || s[j] == '&' || s[j] == '$') {
				return refuse(fmt.Sprintf("a prefixed string or identifier (%s%c…)", s[i:j], s[j]))
			}
			out = append(out, checkTok{tokWord, s[i:j]})
			i = j
		case c == ';':
			out = append(out, checkTok{tokPunct, ";"})
			i++
		case strings.IndexByte(checkPunct, c) >= 0:
			out = append(out, checkTok{tokPunct, string(c)})
			i++
		default:
			if lenient {
				i++
				continue
			}
			return refuse(fmt.Sprintf("%q outside a quoted string", string(c)))
		}
	}
	return out, nil
}

func isCheckWordStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isCheckWordByte(c byte) bool {
	return isCheckWordStart(c) || (c >= '0' && c <= '9')
}

// CheckCountSQL is the query an applier runs for a validated check: a
// count-shape probe (`SELECT count(*) FROM … LIMIT 1`) as it is, its one value
// the violating-row count; a row-shape probe (the unique-index `SELECT <cols>,
// count(*) … GROUP BY … HAVING …`) wrapped so each row it returns counts —
// the same reading the dev sync's guard gives them.
func CheckCountSQL(sql string) string {
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	if strings.HasPrefix(strings.ToLower(body), "select count(*) from") {
		return body
	}
	return "SELECT count(*) FROM (" + body + ") AS w17_check"
}
