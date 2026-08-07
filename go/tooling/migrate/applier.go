// Package migrate is the per-connection migration-apply orchestration
// the w17migrate binary drives. It owns the Plan → verify → apply →
// record loop, but is dialect-neutral: per-target execution is
// delegated to an `Applier` whose concrete implementations
// (PG / MySQL / SQLite over `database/sql`; Redis / NATS / S3
// over per-dialect SDKs) land in Phase E together with the
// e2e harness migration.
//
// D30 adapter pattern: orchestration (this package) is pure
// orchestration. No I/O beyond what the Client + Applier +
// lock-package perform; no dialect-aware logic. Concrete
// per-dialect packages (`migrate/internal/postgres`,
// `migrate/internal/mysql`, …) plug into the `ApplierFor` factory
// via `migrate/factory`.
package migrate

import (
	"context"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// Applier is the per-connection execution surface. One impl per
// target type (PG / MySQL / SQLite / Redis / NATS / S3). Apply
// receives a Migration whose `up_sql` (or, for non-SQL targets,
// whatever the dialect emits as the apply payload) has already
// been verified against `content_sha256` by the orchestrator —
// the impl just executes.
type Applier interface {
	// Apply executes the migration's apply payload against the
	// target. Returns the error verbatim with whatever context
	// the dialect can attach (e.g. PG's failing statement, MySQL's
	// errno) so deploy logs surface the why without re-running.
	Apply(ctx context.Context, m *applyfetchpb.Migration) error

	// Rollback executes the migration's down payload against the
	// target — the inverse of Apply. Order: down_pre_tx (post-tx
	// equivalent for the down direction; e.g. DROP INDEX
	// CONCURRENTLY) first, then down_sql (the in-tx body that
	// includes the wc_migrations DELETE applied.Wrap injected).
	//
	// Down bodies are produced by applied.Wrap, which places the
	// `DELETE FROM wc_migrations WHERE timestamp = '<id>'` erase at the
	// position that is safe for the dialect's atomicity (C3/AAW1): at the
	// HEAD of the in-tx down for atomic-DDL SQL (PG), but LAST — after the
	// rollback body — for non-atomic-DDL (MySQL, whose DDL implicit-commits)
	// and for KV stores (no transaction). An Applier MUST stream the down
	// bodies VERBATIM in order (down_pre_tx then down_sql); it must NOT
	// re-derive or reposition the erase, or it reintroduces the C3
	// false-clear bug (the ledger recording a rollback that didn't finish).
	//
	// Errors propagate verbatim; partial-rollback recovery is the
	// operator's problem (same posture as Apply).
	Rollback(ctx context.Context, m *applyfetchpb.Migration) error

	// AppliedHead returns the id of the most recently applied
	// migration on the target (D27 `wc_migrations`). Empty string
	// = nothing applied yet (fresh DB / connection). Used by the
	// offline orchestrator (D-iter3-7) as the lower bound when
	// computing pending = filesystem ∩ (id > head, id ≤ target).
	//
	// Implementations:
	//   - SQL dialects (PG / MySQL / SQLite): query
	//     `SELECT max(timestamp) FROM wc_migrations`. Missing
	//     table = empty string (treated as fresh DB).
	//   - Non-SQL dialects (Redis / NATS / S3): per-dialect
	//     state-object lookup (Redis hash `wc:migrations`, a NATS
	//     KV bucket, S3 marker objects) — all implemented; a
	//     missing store reads as a fresh DB (empty string).
	AppliedHead(ctx context.Context) (string, error)

	// Close releases any per-connection resources (DB pool,
	// gRPC conn, S3 client, …). Idempotent.
	Close() error
}

// ApplierFor produces an Applier for a connection name. The
// orchestrator calls it once per connection it has pending
// migrations for. Returning an error here aborts the whole apply
// (the orchestrator does NOT silently skip connections without a
// configured target — partial-apply ambiguity is worse than
// loud refusal).
//
// Production w17migrate builds a factory from `--target` flags
// (one per connection). Tests inject a stub factory directly.
type ApplierFor func(connectionName string) (Applier, error)

// Phase classifies how far a single migration has been applied on
// the target — the Q52 two-phase ledger state read from
// `wc_migrations.post_tx_complete`.
type Phase int

const (
	// PhaseFresh — no applied-state row for this migration (it has
	// not started, or the target is a fresh DB). Apply runs in full.
	PhaseFresh Phase = iota
	// PhasePending — the in-tx half committed (its row exists, marked
	// post_tx_complete=false) but the post-tx skirt did not finish.
	// A prior deploy crashed between the in-tx COMMIT and the
	// completing UPDATE. Resume runs ONLY the post-tx half; re-running
	// the already-committed in-tx DDL would wedge ("relation already
	// exists").
	PhasePending
	// PhaseComplete — the row exists and post_tx_complete=true; the
	// migration is fully applied. (Pending-from-Plan migrations are
	// never Complete — AppliedHead's post_tx_complete filter keeps a
	// complete row at/under the head cutoff.)
	PhaseComplete
)

// ResumableApplier is the optional Q52 capability a dialect
// implements when its skirt path tracks per-migration phase for
// crash recovery (today only Postgres, via `CREATE INDEX
// CONCURRENTLY`). The orchestrator type-asserts for it: when present
// it reads MigrationPhase before applying and, on PhasePending,
// resumes via ApplyPostTx instead of re-running the full Apply.
// Appliers without it always take the plain Apply path.
type ResumableApplier interface {
	// MigrationPhase reports the applied-state phase of the migration
	// `id` on the target. A missing table / missing row is PhaseFresh
	// (never an error) so a fresh DB reads cleanly.
	MigrationPhase(ctx context.Context, id string) (Phase, error)

	// ApplyPostTx runs ONLY the migration's post-tx half (up_post_tx:
	// the non-transactional skirt ops + the completing UPDATE). Used
	// to resume a PhasePending migration whose in-tx half already
	// committed.
	ApplyPostTx(ctx context.Context, m *applyfetchpb.Migration) error
}

// Wiper is an optional Applier capability: drop ALL of the store's
// user data + schema, leaving it empty — the dev DB lifecycle's
// fresh-build primitive (docs/specs/storage/dev-db-lifecycle.md S7).
// It is deliberately narrow: it wipes only the connected store (not
// other docker volumes), in-place over the live connection (no
// container teardown / readiness dance). Destructive by definition —
// callers gate it (the reconcile only wipes on a fresh branch build,
// after the outgoing branch is snapshotted).
//
// Implemented by the relational dialects (PG / MySQL / SQLite) +
// Redis; schemaless object/stream stores (NATS / S3) don't implement
// it (the reconcile logs + skips them). The orchestrator type-asserts.
type Wiper interface {
	Wipe(ctx context.Context) error
}

// FingerprintCapable is an optional Applier capability: extract
// a deterministic fingerprint of the target DB's current schema
// state (Phase D — D-iter3-14). Relational dialects implement
// it (PG / MySQL / SQLite via information_schema introspection);
// schemaless / non-relational dialects (Redis / NATS / S3)
// don't — `drift detection` doesn't have a meaningful definition
// against schemaless storage. The orchestrator's Phase D check
// type-asserts and skips appliers that don't implement.
//
// Implementations return:
//   - the hex-encoded fingerprint on success;
//   - any error from the underlying introspection — never an
//     empty string + nil error (that's reserved for "no
//     applicable schema" e.g. fresh DB with zero tables).
type FingerprintCapable interface {
	Fingerprint(ctx context.Context) (string, error)
}
