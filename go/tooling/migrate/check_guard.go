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

// ErrCheckRefused marks a check whose SQL is not a single read-only SELECT.
var ErrCheckRefused = errors.New("pre-apply check refused")

// checkDeniedWords may not appear as bare words in a check: statements that
// end or change the transaction or the session, write, take locks, or reach
// outside the database. The planner's probes use none of them; a check that
// does is not one the planner wrote.
var checkDeniedWords = map[string]bool{
	"commit": true, "rollback": true, "begin": true, "start": true, "savepoint": true, "release": true,
	"set": true, "reset": true, "pragma": true, "attach": true, "detach": true, "use": true,
	"insert": true, "update": true, "delete": true, "merge": true, "replace": true, "upsert": true,
	"create": true, "drop": true, "alter": true, "truncate": true, "rename": true, "comment": true,
	"grant": true, "revoke": true, "copy": true, "call": true, "do": true, "execute": true, "exec": true,
	"prepare": true, "deallocate": true, "listen": true, "notify": true, "unlisten": true,
	"lock": true, "unlock": true, "vacuum": true, "analyze": true, "reindex": true, "cluster": true,
	"checkpoint": true, "discard": true, "load": true, "handler": true, "kill": true, "shutdown": true,
	"into": true, "outfile": true, "dumpfile": true, "for": true,
}

// checkDeniedFuncs may not be called by a check: they sleep, signal, lock,
// notify, change settings or touch files (Postgres pg_* administration
// functions are refused as a family).
var checkDeniedFuncs = map[string]bool{
	"sleep": true, "benchmark": true, "get_lock": true, "release_lock": true, "release_all_locks": true,
	"is_free_lock": true, "is_used_lock": true, "load_file": true, "load_extension": true,
	"readfile": true, "writefile": true, "edit": true, "fts3_tokenizer": true,
	"set_config": true, "dblink": true, "dblink_exec": true, "lo_import": true, "lo_export": true,
	"lo_unlink": true, "lo_from_bytea": true, "lo_put": true, "query_to_xml": true,
	"sys_exec": true, "sys_eval": true, "master_pos_wait": true, "source_pos_wait": true,
}

// ValidateCheckSQL accepts only what the planner renders: ONE statement, a
// SELECT, that names no transaction, session, write, lock or file operation.
// Quoted strings, quoted identifiers and comments are skipped; a `;` outside
// them (other than one trailing) is a second statement.
//
// The checks are bound into the migration's content hash, so they are as
// trusted as its body; this is defence in depth for a check that reaches the
// applier anyway.
//
// backslashEscapes says whether the dialect reads a backslash in a quoted
// string as an escape (MySQL by default) or as a character (Postgres with
// standard_conforming_strings, SQLite): the scan must end each string where
// the server does, or a quote it reads as closed would hide a second
// statement. A string with a prefix (E'…', U&'…', X'…', _utf8'…') is refused
// for the same reason.
func ValidateCheckSQL(sql string, backslashEscapes bool) error {
	body := strings.TrimSpace(sql)
	body = strings.TrimSpace(strings.TrimSuffix(body, ";"))
	if body == "" {
		return fmt.Errorf("%w: empty", ErrCheckRefused)
	}
	var words []string
	for i := 0; i < len(body); {
		c := body[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for ; j < len(body); j++ {
				if body[j] == c {
					if j+1 < len(body) && body[j+1] == c {
						j++
						continue
					}
					break
				}
				if c == '\'' && backslashEscapes && body[j] == '\\' {
					j++ // a backslash escape: never ends the string
				}
			}
			if j >= len(body) {
				return fmt.Errorf("%w: an unterminated quote", ErrCheckRefused)
			}
			i = j + 1
		case c == '-' && i+1 < len(body) && body[i+1] == '-', c == '#':
			for i < len(body) && body[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(body) && body[i+1] == '*':
			end := strings.Index(body[i+2:], "*/")
			if end < 0 {
				return fmt.Errorf("%w: an unterminated comment", ErrCheckRefused)
			}
			// MySQL runs /*! … */ "comments": refuse them rather than skip.
			if strings.HasPrefix(body[i+2:], "!") || strings.HasPrefix(body[i+2:], "+") {
				return fmt.Errorf("%w: an executable comment", ErrCheckRefused)
			}
			i += end + 4
		case c == ';':
			return fmt.Errorf("%w: more than one statement", ErrCheckRefused)
		case c == '$' || c == '\\':
			return fmt.Errorf("%w: %q outside a quoted string", ErrCheckRefused, string(c))
		case isCheckWordStart(c):
			j := i
			for j < len(body) && isCheckWordByte(body[j]) {
				j++
			}
			w := strings.ToLower(body[i:j])
			if j < len(body) && (body[j] == '\'' || body[j] == '"' || body[j] == '`') {
				return fmt.Errorf("%w: a prefixed string or identifier (%s%c…)", ErrCheckRefused, body[i:j], body[j])
			}
			words = append(words, w)
			if checkDeniedWords[w] {
				return fmt.Errorf("%w: %q is not a read-only SELECT", ErrCheckRefused, strings.ToUpper(w))
			}
			k := j
			for k < len(body) && (body[k] == ' ' || body[k] == '\t' || body[k] == '\n' || body[k] == '\r') {
				k++
			}
			if k < len(body) && body[k] == '(' && (checkDeniedFuncs[w] || strings.HasPrefix(w, "pg_")) {
				return fmt.Errorf("%w: it calls %s()", ErrCheckRefused, w)
			}
			i = j
		default:
			i++
		}
	}
	if len(words) == 0 || words[0] != "select" {
		return fmt.Errorf("%w: not a SELECT", ErrCheckRefused)
	}
	return nil
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
