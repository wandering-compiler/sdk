// Package factory wires the dialect-specific Applier impls into
// a migrate.ApplierFor that the orchestrator drives. Lives in a
// sub-package (not in `migrate` itself) so the per-dialect packages
// can import the `migrate.Applier` interface for their compile-
// time conformance checks without an import cycle.
package factory

import (
	"context"
	"fmt"
	"strings"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/nats"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/redis"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/s3"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// TargetSpec is one parsed `--target` flag. The CLI accepts the
// form `<connection>=<dsn>` (repeatable). Connection name maps to
// the schema-declared connection (D26); dsn carries the
// per-dialect connection string (postgres://…, redis://…, …).
type TargetSpec struct {
	Connection string
	DSN        string
}

// ParseTargets turns each `name=dsn` string into a TargetSpec.
// Duplicate connection names error — silent override of an
// earlier flag would be a silent footgun for multi-connection
// services with --target supplied twice.
func ParseTargets(flags []string) ([]TargetSpec, error) {
	seen := map[string]struct{}{}
	out := make([]TargetSpec, 0, len(flags))
	for _, raw := range flags {
		eq := strings.Index(raw, "=")
		if eq < 0 {
			return nil, fmt.Errorf("--target %q: expected <connection>=<dsn>", raw)
		}
		name := strings.TrimSpace(raw[:eq])
		dsn := strings.TrimSpace(raw[eq+1:])
		if name == "" {
			return nil, fmt.Errorf("--target %q: connection name is empty", raw)
		}
		if dsn == "" {
			return nil, fmt.Errorf("--target %q: dsn is empty", raw)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("--target %q: connection %q already specified", raw, name)
		}
		seen[name] = struct{}{}
		out = append(out, TargetSpec{Connection: name, DSN: dsn})
	}
	return out, nil
}

// option mutates the factory's per-Applier configuration. Used
// to thread CLI-time settings (parallel override, timeouts, …)
// from main into the dialect Appliers without growing
// FromTargets's positional args.
type option func(*config)

type config struct {
	parallel int // operator-supplied --parallel <N>; 0 = no override
}

// WithParallel sets the operator-supplied `--parallel <N>` CLI
// override. Forwarded to dialect Appliers that implement YAML
// data migrations (Redis, S3 — Phase E v2.1). Other dialects
// silently ignore.
func WithParallel(n int) option {
	return func(c *config) { c.parallel = n }
}

// FromTargets builds an ApplierFor that produces the right
// per-dialect Applier for a given connection name. Dialect is
// inferred from the DSN scheme (`postgres://` →
// postgres.Applier, etc.). options thread CLI-time settings
// (e.g. WithParallel) into Appliers that care.
func FromTargets(specs []TargetSpec, opts ...option) migrate.ApplierFor {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	byName := map[string]TargetSpec{}
	for _, s := range specs {
		byName[s.Connection] = s
	}
	return func(connection string) (migrate.Applier, error) {
		spec, ok := byName[connection]
		if !ok {
			return nil, fmt.Errorf("no --target configured for connection %q", connection)
		}
		switch dialectFromDSN(spec.DSN) {
		case "postgres":
			return postgres.New(context.Background(), spec.DSN)
		case "mysql":
			return mysql.New(context.Background(), spec.DSN)
		case "sqlite":
			return sqlite.New(context.Background(), spec.DSN)
		case "redis":
			a, err := redis.New(context.Background(), spec.DSN)
			if err != nil {
				return nil, err
			}
			if cfg.parallel > 0 {
				a.SetParallelOverride(cfg.parallel)
			}
			return a, nil
		case "nats":
			return nats.New(context.Background(), spec.DSN)
		case "s3":
			a, err := s3.New(context.Background(), spec.DSN)
			if err != nil {
				return nil, err
			}
			if cfg.parallel > 0 {
				a.SetParallelOverride(cfg.parallel)
			}
			return a, nil
		}
		return nil, fmt.Errorf("connection %q: unrecognised DSN scheme in %q (supported: postgres://)",
			connection, spec.DSN)
	}
}

// SnapshotterFromTargets builds a SnapshotterFor that produces the
// right per-dialect Snapshotter for a connection name — the dev DB
// lifecycle's snapshot-tier analogue of FromTargets. Dialect is
// inferred from the same DSN scheme. Stores whose Dump/Restore adapter
// has not landed yet (S2) route to a clear "not yet implemented"
// error rather than a generic unrecognised-scheme one, so a partially
// covered snapshot surfaces the specific gap.
func SnapshotterFromTargets(specs []TargetSpec) migrate.SnapshotterFor {
	byName := map[string]TargetSpec{}
	for _, s := range specs {
		byName[s.Connection] = s
	}
	return func(connection string) (migrate.Snapshotter, error) {
		spec, ok := byName[connection]
		if !ok {
			return nil, fmt.Errorf("no --target configured for connection %q", connection)
		}
		switch dialectFromDSN(spec.DSN) {
		case "postgres":
			return postgres.NewSnapshotter(spec.DSN)
		case "mysql":
			return mysql.NewSnapshotter(spec.DSN)
		case "sqlite":
			return sqlite.NewSnapshotter(spec.DSN)
		case "redis":
			return redis.NewSnapshotter(spec.DSN)
		case "nats":
			return nats.NewSnapshotter(spec.DSN)
		case "s3":
			return s3.NewSnapshotter(spec.DSN)
		}
		return nil, fmt.Errorf("connection %q: unrecognised DSN scheme in %q (supported: postgres://, mysql://, sqlite://, redis://, nats://, s3://)",
			connection, spec.DSN)
	}
}

// SnapshotExt returns the on-disk dump file extension (no dot) for a
// connection's DSN, the snapshot-tier companion to SnapshotterFromTargets.
// SQL dialects dump plain SQL; sqlite is a raw DB image; the schemaless
// stores (redis/nats/s3) dump our gob stream. Unknown schemes get
// "dump" as a neutral fallback (the snapshotter routing errors first
// anyway). Keeping the dialect→ext mapping here keeps all dialect
// knowledge in the factory.
func SnapshotExt(dsn string) string {
	switch dialectFromDSN(dsn) {
	case "postgres", "mysql":
		return "sql"
	case "sqlite":
		return "sqlite"
	case "redis", "nats", "s3":
		return "gob"
	}
	return "dump"
}

// dialectFromDSN maps a DSN scheme to the dialect name
// `apply/<dialect>` packages register under. Recognises the
// connection-string forms each driver accepts (URL-style for PG /
// MySQL / SQLite / Redis; `nats://` for NATS; `s3://` for the
// S3 + minio pattern). Unknown schemes return "" so callers
// route to the unrecognised-DSN error.
func dialectFromDSN(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return "postgres"
	case strings.HasPrefix(dsn, "mysql://"):
		return "mysql"
	case strings.HasPrefix(dsn, "sqlite://"), strings.HasPrefix(dsn, "file:"):
		return "sqlite"
	case strings.HasPrefix(dsn, "redis://"), strings.HasPrefix(dsn, "rediss://"):
		return "redis"
	case strings.HasPrefix(dsn, "nats://"):
		return "nats"
	case strings.HasPrefix(dsn, "s3://"):
		return "s3"
	}
	return ""
}
