// Package cliargv holds the quote-aware command-line tokeniser shared by the
// CLI-style migration backends (internal/redis, internal/nats). Both emit
// shell-ish command lines (`HSET ...`, `nats kv put ...`) with bare tokens,
// double-quoted runs, and single-quoted runs, and parse them with the exact
// same scanner — only the error-message prefix differs. The logic lives here
// so a fix (e.g. teaching it escape sequences) lands once.
package cliargv

import (
	"fmt"
	"strings"
)

// Parse tokenises one command line into argv. Quoted runs are stripped of
// their delimiters; inner content is preserved verbatim. The emitters do not
// produce escape sequences (`\"` / `\'` inside quoted runs), so an unbalanced
// quote is rejected outright rather than partially parsed. Empty /
// whitespace-only input yields a nil argv.
//
// prefix is prepended to the unbalanced-quote error messages so each backend's
// errors stay self-identifying (e.g. "redis ParseArgv" / "nats ParseArgv").
func Parse(prefix, line string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inDouble, inSingle, inToken := false, false, false

	flush := func() {
		if inToken {
			argv = append(argv, cur.String())
			cur.Reset()
			inToken = false
		}
	}

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inDouble:
			if c == '"' {
				inDouble = false
				continue
			}
			cur.WriteByte(c)
			inToken = true
		case inSingle:
			if c == '\'' {
				inSingle = false
				continue
			}
			cur.WriteByte(c)
			inToken = true
		case c == ' ' || c == '\t':
			flush()
		case c == '"':
			inDouble = true
			inToken = true
		case c == '\'':
			inSingle = true
			inToken = true
		default:
			cur.WriteByte(c)
			inToken = true
		}
	}

	if inDouble {
		return nil, fmt.Errorf("%s: unbalanced double quote in %q", prefix, line)
	}
	if inSingle {
		return nil, fmt.Errorf("%s: unbalanced single quote in %q", prefix, line)
	}
	flush()
	return argv, nil
}
