package nats

import (
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/cliargv"
)

// ParseArgv tokenises one nats-CLI-style command line into argv.
// The migrator's `emit/nats` produces shell-style command lines:
//
//   - `nats kv put wc-migrations 20260429T120000Z abc123`
//   - `nats stream rm users`
//   - `nats stream edit users --description "new-name"`
//
// Most tokens are bare; the only quoted runs are the
// double-quoted `--description "value"` form (Go's `%q`
// produces this when the description has spaces or special
// chars). Single quotes don't appear in current emitter
// output but are accepted for parity with the Redis parser.
//
// Empty / whitespace-only input yields a nil argv. Unbalanced
// quotes raise an error rather than passing partial garbage to
// the JetStream API. The scanner itself is shared (see cliargv).
func ParseArgv(line string) ([]string, error) {
	return cliargv.Parse("nats ParseArgv", line)
}
