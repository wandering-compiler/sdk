// Package nats is the production NATS Applier. Talks to NATS
// JetStream via the native Go client (`github.com/nats-io/nats.go`
// + the `jetstream` sub-package); no shell-out, no `nats` CLI dep
// on the deploy host.
//
// The migrator's `emit/nats` produces nats-CLI-style command
// lines — one command per line, with `# wc:` comment markers
// for documentation. The Applier tokenises each line via
// `ParseArgv` and dispatches by subcommand into typed
// JetStream / KV API calls:
//
//   - `nats kv add <bucket>`       → js.CreateKeyValue
//   - `nats kv rm <bucket>`        → js.DeleteKeyValue
//   - `nats kv put <bucket> <k> <v>` → kv.Put
//   - `nats kv del <bucket> <k>`   → kv.Delete
//   - `nats kv purge <bucket>`     → kv.Purge (test-cleanup path)
//   - `nats stream rm <name>`      → js.DeleteStream
//   - `nats stream edit <name> --description <desc>` →
//     js.UpdateStream(StreamConfig{Description: desc})
//
// **Idempotent rollback.** `stream rm` against a missing
// stream and `kv del` / `kv rm` against a missing bucket /
// key are tolerated as soft-success. The migrator's emit
// produces stream-removal lines as the `down` direction of
// AddTable, where the corresponding `up` was a comment-only
// marker (operator creates the stream out-of-band) — so the
// rm runs against a stream that may not exist. Tolerating
// matches production deploy-runbook expectations and replaces
// the test-only `natsTolerant` shim.
//
// DSN form: `nats://[user:pass@]host:port`. Parsed via
// `nats.ParseURL`. Userinfo currently dropped at connect time
// (auth lands in Phase G alongside console-side auth).
//
// **Lazy connect.** `New` validates the DSN + builds the
// Applier without opening a TCP connection — `--dry-run`
// flows that build but never call Apply / AppliedHead don't
// require live connectivity. First Apply / AppliedHead opens
// the connection + JetStream context; subsequent calls reuse.
package nats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// trackerBucket is the well-known JetStream KV bucket name used
// for applied-state bookkeeping (Phase C.3 + applied.NATS()).
// AppliedHead reads its keys; the applied.NATS() Renderer's
// CreateTracker / RecordVersion / RemoveVersion all reference
// it by this exact name.
const trackerBucket = "wc-migrations"

// Applier owns one lazy NATS connection + one JetStream context.
// Concurrent Apply calls share the same connection (NATS
// connections are safe for concurrent use). KV bucket handles
// are cached per-bucket on first reference.
type Applier struct {
	dsn string

	mu      sync.Mutex
	conn    *natsgo.Conn
	js      jetstream.JetStream
	buckets map[string]jetstream.KeyValue
}

var _ migrate.Applier = (*Applier)(nil)

// New parses the DSN + builds a lazy-connect Applier. Returns
// an error only on DSN-shape problems; connection errors are
// deferred to first Apply / AppliedHead.
func New(_ context.Context, dsn string) (*Applier, error) {
	if dsn == "" {
		return nil, fmt.Errorf("nats.New: dsn is empty")
	}
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("nats.New: %w", err)
	}
	return &Applier{dsn: cfg.Server, buckets: map[string]jetstream.KeyValue{}}, nil
}

// connect lazily opens the NATS connection + JetStream context.
// Subsequent calls return the cached pair. Caller holds a.mu.
func (a *Applier) connect() (jetstream.JetStream, error) {
	if a.js != nil {
		return a.js, nil
	}
	nc, err := natsgo.Connect(a.dsn)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", a.dsn, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream context: %w", err)
	}
	a.conn = nc
	a.js = js
	return js, nil
}

// jsContext is the locked accessor for the lazy JetStream context.
func (a *Applier) jsContext() (jetstream.JetStream, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connect()
}

// AppliedHead returns the newest applied-migration timestamp
// from the `wc-migrations` JetStream KV bucket. Empty / missing
// bucket → "" (= "no migrations applied yet").
func (a *Applier) AppliedHead(ctx context.Context) (string, error) {
	js, err := a.jsContext()
	if err != nil {
		return "", err
	}
	kv, err := js.KeyValue(ctx, trackerBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("kv KeyValue %s: %w", trackerBucket, err)
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		// `Keys` on an empty bucket returns ErrNoKeysFound — not
		// an error from our perspective; means nothing applied.
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return "", nil
		}
		return "", fmt.Errorf("kv Keys %s: %w", trackerBucket, err)
	}
	var head string
	for _, k := range keys {
		if k > head {
			head = k
		}
	}
	return head, nil
}

// Apply runs the migration body lines through the typed
// JetStream / KV API.
func (a *Applier) Apply(ctx context.Context, m *applyfetchpb.Migration) error {
	if err := a.run(ctx, m.GetUpSql()); err != nil {
		return fmt.Errorf("nats apply up_sql: %w", err)
	}
	if pt := m.GetUpPostTx(); pt != "" {
		if err := a.run(ctx, pt); err != nil {
			return fmt.Errorf("nats apply up_post_tx: %w", err)
		}
	}
	return nil
}

// Rollback runs the migration's down payload. Order: down_pre_tx
// first, then down_sql. applied.NATS() injects the
// `nats kv del wc-migrations <ts>` bookkeeping erase into the
// down body; user-side down ops (stream rm / kv del / etc.)
// live alongside and are tolerated as missing per the existing
// run() rules.
func (a *Applier) Rollback(ctx context.Context, m *applyfetchpb.Migration) error {
	if err := a.run(ctx, m.GetDownPreTx()); err != nil {
		return fmt.Errorf("nats rollback down_pre_tx: %w", err)
	}
	if err := a.run(ctx, m.GetDownSql()); err != nil {
		return fmt.Errorf("nats rollback down_sql: %w", err)
	}
	return nil
}

// Close shuts the lazy NATS connection (no-op if never opened).
func (a *Applier) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
		a.js = nil
		a.buckets = map[string]jetstream.KeyValue{}
	}
	return nil
}

// run iterates each line of script + dispatches via runOne.
// Empty lines + `#` comments are skipped (matches the
// migrator's emit hygiene).
func (a *Applier) run(ctx context.Context, script string) error {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := a.runOne(ctx, line); err != nil {
			return err
		}
	}
	return nil
}

// runOne tokenises one command line + dispatches to the typed
// API. Lines may begin with the literal `nats` token (the
// migrator emits the full CLI form); we strip it.
func (a *Applier) runOne(ctx context.Context, line string) error {
	argv, err := ParseArgv(line)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return nil
	}
	if argv[0] == "nats" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return nil
	}

	js, err := a.jsContext()
	if err != nil {
		return err
	}

	switch argv[0] {
	case "kv":
		return a.runKv(ctx, js, argv[1:])
	case "stream":
		return a.runStream(ctx, js, argv[1:])
	default:
		return fmt.Errorf("nats: unsupported subcommand %q in %q", argv[0], line)
	}
}

// runKv dispatches `kv {add,rm,put,del,purge} ...` lines.
func (a *Applier) runKv(ctx context.Context, js jetstream.JetStream, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("nats kv: missing subcommand")
	}
	sub := argv[0]
	rest := argv[1:]
	switch sub {
	case "add":
		if len(rest) < 1 {
			return fmt.Errorf("nats kv add: missing bucket name")
		}
		_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: rest[0]})
		if err != nil {
			// Bucket-already-exists is non-fatal: re-applies of
			// the initial migration must succeed when the bucket
			// is already there.
			if errors.Is(err, jetstream.ErrBucketExists) ||
				strings.Contains(err.Error(), "stream name already in use") {
				return nil
			}
			return fmt.Errorf("kv add %s: %w", rest[0], err)
		}
		return nil

	case "rm":
		if len(rest) < 1 {
			return fmt.Errorf("nats kv rm: missing bucket name")
		}
		if err := js.DeleteKeyValue(ctx, rest[0]); err != nil {
			if errors.Is(err, jetstream.ErrBucketNotFound) {
				return nil
			}
			return fmt.Errorf("kv rm %s: %w", rest[0], err)
		}
		a.invalidateBucket(rest[0])
		return nil

	case "put":
		if len(rest) < 3 {
			return fmt.Errorf("nats kv put: expected `<bucket> <key> <value>`, got %v", rest)
		}
		kv, err := a.bucket(ctx, js, rest[0])
		if err != nil {
			return err
		}
		if _, err := kv.Put(ctx, rest[1], []byte(rest[2])); err != nil {
			return fmt.Errorf("kv put %s/%s: %w", rest[0], rest[1], err)
		}
		return nil

	case "del":
		if len(rest) < 2 {
			return fmt.Errorf("nats kv del: expected `<bucket> <key>`, got %v", rest)
		}
		kv, err := a.bucket(ctx, js, rest[0])
		if err != nil {
			if errors.Is(err, jetstream.ErrBucketNotFound) {
				return nil
			}
			return err
		}
		if err := kv.Delete(ctx, rest[1]); err != nil &&
			!errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("kv del %s/%s: %w", rest[0], rest[1], err)
		}
		return nil

	case "purge":
		// Test-cleanup path: drop every key in a bucket without
		// removing the bucket itself.
		if len(rest) < 1 {
			return fmt.Errorf("nats kv purge: missing bucket name")
		}
		kv, err := a.bucket(ctx, js, rest[0])
		if err != nil {
			if errors.Is(err, jetstream.ErrBucketNotFound) {
				return nil
			}
			return err
		}
		keys, err := kv.Keys(ctx)
		if err != nil {
			if errors.Is(err, jetstream.ErrNoKeysFound) {
				return nil
			}
			return fmt.Errorf("kv keys %s: %w", rest[0], err)
		}
		for _, k := range keys {
			if err := kv.Purge(ctx, k); err != nil &&
				!errors.Is(err, jetstream.ErrKeyNotFound) {
				return fmt.Errorf("kv purge %s/%s: %w", rest[0], k, err)
			}
		}
		return nil

	default:
		return fmt.Errorf("nats kv: unsupported subcommand %q", sub)
	}
}

// runStream dispatches `stream {rm,edit,ls} ...` lines. `ls`
// is a no-op for the apply path (it's a query, not a mutation;
// container test helpers can use the API directly).
func (a *Applier) runStream(ctx context.Context, js jetstream.JetStream, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("nats stream: missing subcommand")
	}
	sub := argv[0]
	rest := argv[1:]
	switch sub {
	case "rm":
		// `stream rm [--force] <name>` — the --force flag the
		// emitter or test harness may inject is the CLI's way of
		// suppressing the interactive prompt; the API call has
		// no equivalent (no prompt). Strip --force if present
		// and the next token is the stream name.
		args := dropFlag(rest, "--force")
		if len(args) < 1 {
			return fmt.Errorf("nats stream rm: missing stream name")
		}
		if err := js.DeleteStream(ctx, args[0]); err != nil {
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				return nil
			}
			return fmt.Errorf("stream rm %s: %w", args[0], err)
		}
		return nil

	case "edit":
		// Minimal parsing: `stream edit <name> --description <desc>`.
		// The migrator no longer emits this shape (a QUEUE-model
		// rename is refused at emit — a JetStream stream can't be
		// renamed), but the handler is kept for hand-authored /
		// operator-supplied edit lines. Other --flags would extend
		// this switch.
		if len(rest) < 1 {
			return fmt.Errorf("nats stream edit: missing stream name")
		}
		name := rest[0]
		flags := rest[1:]
		desc, ok := flagValue(flags, "--description")
		if !ok {
			return fmt.Errorf("nats stream edit: only --description supported")
		}
		// Read existing config, mutate description, write back.
		// A `stream edit` targeting a nonexistent stream is a REAL
		// failure, not a tolerated no-op: unlike `stream rm` (whose
		// idempotent-missing tolerance backs the AddTable-down path),
		// an edit against a missing stream can only mean the target
		// drifted or the body is wrong — swallowing it would report
		// a mutation as applied when nothing was mutated. Surface it.
		s, err := js.Stream(ctx, name)
		if err != nil {
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				return fmt.Errorf("stream edit %s: stream does not exist", name)
			}
			return fmt.Errorf("stream lookup %s: %w", name, err)
		}
		cfg := s.CachedInfo().Config
		cfg.Description = desc
		if _, err := js.UpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("stream edit %s: %w", name, err)
		}
		return nil

	case "ls":
		return nil // query op; nothing to apply
	default:
		return fmt.Errorf("nats stream: unsupported subcommand %q", sub)
	}
}

// bucket returns a cached KV handle for `name`, opening it on
// first reference.
func (a *Applier) bucket(ctx context.Context, js jetstream.JetStream, name string) (jetstream.KeyValue, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if kv, ok := a.buckets[name]; ok {
		return kv, nil
	}
	kv, err := js.KeyValue(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("kv KeyValue %s: %w", name, err)
	}
	a.buckets[name] = kv
	return kv, nil
}

// invalidateBucket evicts a cached KV handle. Called after a
// `kv rm` so the next reference re-opens (or fails clean).
func (a *Applier) invalidateBucket(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.buckets, name)
}

// dropFlag returns argv with all instances of `flag` removed.
// Only handles bare flags (no `--flag=value` form, no
// `--flag value` paired form) — the CLI only injects
// `--force`, which is bare.
func dropFlag(argv []string, flag string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == flag {
			continue
		}
		out = append(out, a)
	}
	return out
}

// flagValue scans argv for `--flag <value>` and returns the
// value. Used for `--description "<desc>"`-style options.
func flagValue(argv []string, flag string) (string, bool) {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag {
			return argv[i+1], true
		}
	}
	return "", false
}
