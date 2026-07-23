package redis

import (
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/cliargv"
)

// ParseArgv tokenises one redis-cli-style command line into argv.
// The migrator's `emit/redis` produces three token shapes:
//
//   - bare tokens — `HSET wc:migrations 20260429T120000Z abc123`
//   - double-quoted scripts — `EVAL "local cursor='0';…" 0 'pat:*'`
//     (Lua bodies; single quotes inside are literal Lua strings)
//   - single-quoted args — `'users:*'` (key patterns)
//
// Quoted runs are stripped of their delimiters; the inner content
// is preserved verbatim. The emitter does not produce escape
// sequences (`\"` / `\'` inside quoted runs), so this parser
// rejects unbalanced quotes outright instead of trying to handle
// them — if the emitter ever needs escapes, teach the shared
// scanner (see cliargv), with explicit test coverage.
//
// Empty input yields a nil argv. Whitespace-only input also
// yields nil — callers (Apply.run) treat both as no-op lines.
func ParseArgv(line string) ([]string, error) {
	return cliargv.Parse("redis ParseArgv", line)
}
