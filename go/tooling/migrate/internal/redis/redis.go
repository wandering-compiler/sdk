// Package redis is the production Redis Applier. Talks to Redis
// via the native Go client (`github.com/redis/go-redis/v9`); no
// shell-out, no `redis-cli` dep on the deploy host.
//
// The migrator's `emit/redis` produces redis-cli-style command
// text — one command per line, with bare tokens / double-quoted
// Lua scripts / single-quoted key patterns. The Applier
// tokenises each line via `ParseArgv` and dispatches it through
// the typed client's `Do(ctx, args...)` low-level call, which
// accepts any Redis command as a variadic argv. EVAL scripts go
// through the same path — `Do(ctx, "EVAL", script, numkeys,
// keys..., args...)` is the canonical shape.
//
// DSN form: `redis://[user:pass@]host[:port][/db][?param=value]`
// (or `rediss://…` for TLS). `redis.ParseURL` from go-redis
// handles every nuance — userinfo, query params, db index — so
// we don't roll our own parsing.
//
// Comment filtering: emit drops `# wc:` markers as no-op
// audit-trail entries. Redis would reject them as commands; we
// strip via FilterComments before tokenising.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/datamigrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Applier wraps a single go-redis client. Lazy-connect: New
// parses the DSN + builds the client (which doesn't open a
// connection until first command), so `--dry-run` flows that
// build the Applier but never call Apply don't require live
// connectivity.
type Applier struct {
	client *goredis.Client

	// parallelOverride is the operator-supplied
	// `--parallel <N>` from the CLI. 0 = no override; YAML
	// data migrations fall back to the migration's project-
	// side `parallel:` field, then to datamigrate.DefaultParallel.
	// Only meaningful for YAML data migrations (Phase E v2.1);
	// command-style bodies ignore it.
	parallelOverride int

	// Run-lock tunables (Q48-datamigrate-1). 0 = use the runlock
	// package defaults; tests set them small to exercise the
	// heartbeat / TTL paths without real-time waits.
	lockTTL  time.Duration
	lockBeat time.Duration
}

var _ migrate.Applier = (*Applier)(nil)
var _ migrate.Wiper = (*Applier)(nil)

// New parses the DSN into go-redis options + builds the client.
// Returns a usable Applier even when the server is unreachable
// — the first Apply / AppliedHead surfaces the connection error
// (matches the previous redis-cli shape, where the exec failed
// at command time).
func New(_ context.Context, dsn string) (*Applier, error) {
	if dsn == "" {
		return nil, fmt.Errorf("redis.New: dsn is empty")
	}
	opts, err := goredis.ParseURL(dsn)
	if err != nil {
		return nil, fmt.Errorf("redis.New: %w", err)
	}
	return &Applier{client: goredis.NewClient(opts)}, nil
}

// AppliedHead returns the newest applied-migration timestamp by
// reading `HKEYS wc:migrations` and picking the max-by-lex
// member. The bookkeeping hash is populated by
// `applied.Redis()` (Phase C.2): every successful migration
// HSETs `wc:migrations <ts> <hex(sha256)>`. Empty / missing
// hash → empty string (= "no migrations applied yet"), which
// the orchestrator interprets as "run every pending
// migration". w17's YYYYMMDDTHHMMSSZ id format is lex-sortable
// == chrono-sortable, so max-lex == newest.
func (a *Applier) AppliedHead(ctx context.Context) (string, error) {
	keys, err := a.client.HKeys(ctx, "wc:migrations").Result()
	if err != nil {
		// HKeys against a missing key yields an empty slice, not
		// an error — anything coming through here is a real
		// connection / auth problem worth surfacing.
		return "", fmt.Errorf("redis HKEYS wc:migrations: %w", err)
	}
	var head string
	for _, k := range keys {
		if k > head {
			head = k
		}
	}
	return head, nil
}

// Apply pipes the migration's command bodies through the Redis
// client. up_post_tx is honoured the same way as up_sql (Redis
// has no transactional vs non-transactional distinction; the
// migrator emits both fields uniformly).
//
// Phase E (D-iter3-15): bodies whose up_sql is a YAML data
// migration dispatch through applyYAMLDataMigration instead —
// per-key SCAN+GET+modify+SET loop driven by the parsed
// Operations[]. Detection via lib/datamigrate.LooksLikeYAML.
func (a *Applier) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	if datamigrate.LooksLikeYAML([]byte(m.GetUpSql())) {
		return a.applyYAMLDataMigration(ctx, m)
	}
	if err := a.run(ctx, m.GetUpSql()); err != nil {
		return fmt.Errorf("redis apply up_sql: %w", err)
	}
	if pt := m.GetUpPostTx(); pt != "" {
		if err := a.run(ctx, pt); err != nil {
			return fmt.Errorf("redis apply up_post_tx: %w", err)
		}
	}
	return nil
}

// Rollback runs the migration's down payload. Order: down_pre_tx
// first (per the apply-direction inverse), then down_sql.
// applied.Redis() injects the HDEL bookkeeping erase into
// down_sql; the user's down body (e.g. SCAN+UNLINK) lives
// alongside.
//
// Phase E: YAML data migration bodies (forward up_sql is YAML)
// dispatch through rollbackYAMLDataMigration. The down body is
// either an auto-derived YAML inverse (RENAME swap, ADD
// reverses to REMOVE) or a `# wc:irreversible:` comment block
// — the latter refuses without --allow-irreversible.
func (a *Applier) Rollback(ctx context.Context, m *applyfetchpb.Migration) error {
	if datamigrate.LooksLikeYAML([]byte(m.GetUpSql())) {
		return a.rollbackYAMLDataMigration(ctx, m)
	}
	if err := a.run(ctx, m.GetDownPreTx()); err != nil {
		return fmt.Errorf("redis rollback down_pre_tx: %w", err)
	}
	if err := a.run(ctx, m.GetDownSql()); err != nil {
		return fmt.Errorf("redis rollback down_sql: %w", err)
	}
	return nil
}

// Wipe flushes the connected logical DB (migrate.Wiper) — the dev
// fresh-build primitive. FLUSHDB drops every key in this DB only (not
// the whole server), leaving it empty.
func (a *Applier) Wipe(ctx context.Context) error {
	if err := a.client.FlushDB(ctx).Err(); err != nil {
		return fmt.Errorf("redis Wipe: %w", err)
	}
	return nil
}

// Close shuts the go-redis client + its connection pool.
func (a *Applier) Close() error {
	if a.client == nil {
		return nil
	}
	return a.client.Close()
}

// SetParallelOverride installs the operator's `--parallel <N>`
// CLI value. Forwarded by the factory at construction. 0 = no
// override (fall through to the YAML migration's `parallel:`
// field, then datamigrate.DefaultParallel).
//
// Only consulted by YAML data migrations (Phase E v2.1); the
// command-style migration path runs sequentially regardless.
func (a *Applier) SetParallelOverride(n int) {
	a.parallelOverride = n
}

// run tokenises script line-by-line and dispatches each via the
// generic Do(ctx, args...) call. Empty / comment-only lines are
// skipped (matches the redis-cli stdin behaviour: blank lines
// are no-ops). Unbalanced-quote errors from ParseArgv surface
// as Apply errors with the offending line included.
func (a *Applier) run(ctx context.Context, script string) error {
	cmds := FilterComments(script)
	if cmds == "" {
		return nil
	}
	for _, line := range strings.Split(cmds, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		argv, err := ParseArgv(line)
		if err != nil {
			return fmt.Errorf("parse: %w", err)
		}
		if len(argv) == 0 {
			continue
		}
		if err := a.do(ctx, argv); err != nil {
			return fmt.Errorf("exec %q: %w", line, err)
		}
	}
	return nil
}

// do dispatches one argv to the typed client. go-redis's
// generic Do accepts `...interface{}` — we wrap each string
// token in an interface so the variadic call lines up.
//
// `Nil` results are treated as success: e.g. `HDEL` on a
// missing field returns redis.Nil from the client, which is the
// expected "nothing to delete" path.
func (a *Applier) do(ctx context.Context, argv []string) error {
	args := make([]any, len(argv))
	for i, s := range argv {
		args[i] = s
	}
	if err := a.client.Do(ctx, args...).Err(); err != nil {
		if errors.Is(err, goredis.Nil) {
			return nil
		}
		return err
	}
	return nil
}

// FilterComments strips `# wc:` no-op marker lines + blank
// lines from a script. Exposed for tests + for any callers
// that want the same hygiene.
func FilterComments(script string) string {
	var out strings.Builder
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}
