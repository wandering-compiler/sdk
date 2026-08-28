package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"google.golang.org/protobuf/encoding/protojson"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// ConnTarget pins one connection's apply ceiling — the lock's
// per-connection target_migration_id (+ its content hash for a keyless
// integrity check). The caller (the apply CLI) reads these from its local
// lock and passes them in, so the apply tier depends on no lock proto
// (public-split: the apply tier moves into w17ctl's own module, importing
// zero private srcgo).
type ConnTarget struct {
	// Connection is the connection name (the lock connection's name).
	Connection string
	// TargetMigrationID is the highest migration id to apply for this
	// connection (the lock's target_migration_id). Empty = unpinned → skipped.
	TargetMigrationID string
	// TargetContentSha256 is the lock's pinned content hash for the target
	// migration; when set it must match the fetched artifact (refuses a
	// hand-edited body). Empty = no hash pin.
	TargetContentSha256 string
}

// Config pins everything Run needs to drive an offline apply
// (D-iter3-6). Built from CLI flags (production) or directly
// (tests). The lock pins per-connection target_migration_id;
// MigrationsDir holds the artifacts fetched by `w17migrate
// fetch`; ApplierFor opens per-connection drivers; the DB-side
// `wc_migrations` table (D27) is the source of truth for what
// is already applied.
type Config struct {
	// Targets declare the deploy ceiling per connection (read from the
	// caller's lock). Read-only at apply time — never written back
	// (D-iter3-7: lock is pin, not cursor).
	Targets []ConnTarget

	// MigrationsDir is the filesystem root holding artifacts
	// `fetch` wrote (default: `w17/migrations`).
	// Layout: <MigrationsDir>/<connection>/<id>.json (canonical
	// protojson Migration) + <id>.up.sql + <id>.down.sql
	// (informational copies for operator audit).
	MigrationsDir string

	// ApplierFor opens a per-connection driver. Production wires
	// per-dialect Appliers via the factory package; tests inject
	// stubs.
	ApplierFor ApplierFor

	// Out is the progress + dry-run sink. Defaults to
	// io.Discard when nil.
	Out io.Writer

	// DryRun = true: load + verify + filter; print pending; do
	// NOT call Applier.Apply.
	DryRun bool

	// AdoptConnections, when non-empty, narrows [RunAdopt] to these
	// connections. Empty = every connection the lock pins.
	//
	// It exists because adoption is per-connection by NATURE and the
	// project is not: a relational connection whose migration CREATEs
	// tables must be adopted onto an existing database, while a KV
	// connection's migration introduces nothing durable (a redis keyspace
	// is made by writing a key, so its `up_sql` is comments) and wants an
	// ordinary apply instead. Without this the operator could only ask for
	// both and get neither.
	AdoptConnections []string

	// LogFormat selects the per-migration log shape:
	//   ""     / "text" — human-readable (default)
	//   "json"          — one JSON line per migration via slog
	// JSON enables CI / SIEM ingest without parsing free-form
	// text.
	LogFormat string
}

// Pending is one migration the orchestrator wants to apply (or
// would apply, in dry-run). Returned by Plan so callers can
// inspect / count without driving Apply.
type Pending struct {
	Connection string
	Migration  *applyfetchpb.Migration
	// Adopt marks a squash baseline this database has ALREADY satisfied:
	// its applied head is one of the migrations the baseline replaces, so
	// the schema it describes is the schema in front of us. Apply records
	// it in wc_migrations and runs none of its DDL.
	//
	// Without this a squashed project's next deploy runs the baseline's
	// full CREATE against tables that exist. It is a separate field rather
	// than a filter because the row still has to be WRITTEN — a database
	// that skipped the baseline silently would offer it again on every
	// subsequent deploy, forever.
	Adopt bool
}

// RollbackConfig pins everything RunRollback needs to drive the
// inverse-apply path. Mirrors Config except `ToMigrationID`
// names the LOWEST id to keep applied (anything strictly newer
// gets rolled back). Lock + MigrationsDir + ApplierFor reuse the
// fetch-side artifacts; rollback is offline same as apply.
type RollbackConfig struct {
	Targets       []ConnTarget
	MigrationsDir string
	ApplierFor    ApplierFor
	Out           io.Writer
	DryRun        bool
	LogFormat     string // "" / "text" / "json" — see Config.LogFormat

	// ToMigrationID is the highest id to KEEP. Every applied
	// migration with id strictly > ToMigrationID rolls back.
	// Empty string = roll back everything currently applied
	// (reset to fresh DB).
	ToMigrationID string
}

// Plan walks every connection in the lock, opens each connection's
// Applier to query AppliedHead (DB-side cutoff per D-iter3-7),
// loads filesystem migrations from MigrationsDir, and selects the CHAIN
// leading to the pinned target — `chainFromTarget` walks `prev_content_sha256`
// links backwards from the pin, and an artifact in range that is not on that
// chain is REFUSED rather than skipped. The applied-head cutoff then trims
// what is already in the database.
//
// The `filesystem ∩ (id > applied_head, id ≤ target)` formula this described
// until T2-5 pass #14 (D14-2) IS B11-1: id-range selection over whatever the
// directory holds is what let an inserted off-chain artifact apply. Two other
// contract surfaces still carried it; a second reader implementing the formula
// would reintroduce the defect wholesale.
//
// Each artifact's content_sha256 is recomputed and verified — over SEVEN
// inputs (all four SQL segments, `prev`, `supersedes`, `adopt_sql`), not the
// up_sql body alone.
//
// Connections walk in lex name order (D41). Plan opens an Applier
// per connection to query AppliedHead, then closes it; Run reopens
// for the actual Apply loop. Two opens per deploy is acceptable
// (driver init is cheap).
func Plan(ctx context.Context, cfg Config) ([]Pending, error) {
	if cfg.MigrationsDir == "" {
		return nil, fmt.Errorf("migrate.Plan: MigrationsDir is empty")
	}
	if cfg.ApplierFor == nil {
		return nil, fmt.Errorf("migrate.Plan: ApplierFor is nil")
	}

	targets := append([]ConnTarget(nil), cfg.Targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].Connection < targets[j].Connection })

	var out []Pending
	for _, ct := range targets {
		target := ct.TargetMigrationID
		if target == "" {
			// No target pinned — operator hasn't pushed a schema
			// for this connection yet. Skip; Run prints a notice.
			continue
		}

		diskMigs, err := loadConnectionMigrations(cfg.MigrationsDir, ct.Connection)
		if err != nil {
			return nil, fmt.Errorf("connection %s: %w", ct.Connection, err)
		}

		// Verify the target id is on disk + lock hash matches.
		var found *applyfetchpb.Migration
		for _, m := range diskMigs {
			if m.GetId() == target {
				found = m
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("connection %s: target_migration_id %q not found in %s — run `migrate fetch` first",
				ct.Connection, target, filepath.Join(cfg.MigrationsDir, ct.Connection))
		}
		// T25-D2-1: a pinned target MUST carry a content hash. ContentHash always
		// yields a non-empty value, so an empty pin on a set target is an anomaly
		// (a hand-edit that blanked it, or a lock predating the pin) — and the
		// original `want != "" && …` skipped the whole tamper check for it,
		// failing OPEN: a tampered artifact would apply unverified. The offline
		// client does not re-verify the signature (D4), so this content pin is the
		// control that catches a tampered fetched artifact — it must fail closed.
		if ct.TargetContentSha256 == "" {
			return nil, fmt.Errorf("connection %s: target_migration_id %q is pinned but target_content_sha256 is empty — refusing apply (the content-integrity check cannot run; regenerate the lock, a valid lock always carries the hash)",
				ct.Connection, target)
		}
		if want := ct.TargetContentSha256; want != found.GetContentSha256() {
			return nil, fmt.Errorf("connection %s: lock target_content_sha256=%s ≠ artifact %s for migration %q (someone hand-edited; refusing apply)",
				ct.Connection, want, found.GetContentSha256(), target)
		}

		// Query DB-side cutoff.
		applier, err := cfg.ApplierFor(ct.Connection)
		if err != nil {
			return nil, fmt.Errorf("connection %s: applier: %w", ct.Connection, err)
		}
		head, err := applier.AppliedHead(ctx)
		closeErr := applier.Close()
		if err != nil {
			return nil, fmt.Errorf("connection %s: AppliedHead: %w", ct.Connection, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("connection %s: Close after AppliedHead: %w", ct.Connection, closeErr)
		}

		// The applicable set is the CHAIN that ends at the target, not
		// every file in the id range (T2-5 B11-1).
		//
		// Selecting by id range is what let a migration nobody vouched for
		// execute: the lock pins one hash, the target's, and an artifact in
		// the range only ever had to hash to its own content_sha256 — which
		// whoever holds the directory recomputes. Walking back from the
		// target through prev_content_sha256 inverts that: the pinned hash
		// covers the target, the target covers its predecessor, and so on,
		// so a file that no link points at is not on the chain and is never
		// executed — including one INSERTED into the range, which the old
		// filter would have run.
		chain, err := chainFromTarget(diskMigs, found)
		if err != nil {
			return nil, fmt.Errorf("connection %s: %w", ct.Connection, err)
		}

		// A target that roots its own chain is legitimate (first migration,
		// or a squash baseline) — unless artifacts BELOW it are also
		// pending, which means the set predates chaining and the walk just
		// silently dropped them. See unchainedTargetRefusal.
		if found.GetPrevContentSha256() == "" {
			below := 0
			for _, m := range diskMigs {
				if m.GetId() >= target {
					continue
				}
				if head != "" && m.GetId() <= head {
					continue
				}
				below++
			}
			if below > 0 {
				return nil, unchainedTargetRefusal(ct.Connection, found, below)
			}
		}

		// Filter pending. chain is ordered oldest→newest and ends at the
		// target, so the upper bound is structural now; head=="" →
		// everything on the chain, otherwise strict-after head.
		for _, m := range chain {
			if head != "" && m.GetId() <= head {
				continue
			}
			// A squash baseline whose superseded set contains this
			// database's applied head describes a schema this database
			// already has. Record it, run nothing.
			//
			// Deliberately keyed on the HEAD rather than on "any applied
			// id": the head is the one fact the target database reports,
			// and it is exactly the question being asked — is this
			// database at a point the baseline replaces? A database that
			// never applied any of the superseded set has head=="" or an
			// unrelated id, falls through, and gets the real CREATE.
			//
			// And the head must be the LAST id the baseline collapsed, not
			// merely one of them (T2-5 pass #15, T25-A15-5). A database
			// mid-range applied a PREFIX of the collapsed set, so the
			// remainder is exactly the DDL it still needs — adopting there
			// records the baseline as done and runs none of it, and the
			// superseded rows are never served again, so the gap is
			// permanent and silent. Refuse instead: the operator can raise
			// the database to the end of the range (or rebuild it) and
			// deploy again, and neither choice is one this tool may make on
			// its own.
			adopt := false
			if head != "" && supersedes(m, head) {
				last := lastSuperseded(m)
				if head != last {
					return nil, fmt.Errorf("connection %s: refusing to adopt squash baseline %s — this database is at %s, which the baseline replaces, but %s is the LAST migration it collapsed. Adopting would record the baseline while never running the DDL between %s and %s, and those migrations are no longer served. Bring the database up to %s before deploying the squash, or rebuild it from the baseline",
						ct.Connection, m.GetId(), head, last, head, last, last)
				}
				adopt = true
			}
			out = append(out, Pending{
				Connection: ct.Connection,
				Migration:  m,
				Adopt:      adopt,
			})
		}
	}
	return out, nil
}

// Run plans + applies. On dry-run it stops after Plan and prints
// each pending migration's metadata + up_sql + up_post_tx (the
// full set of statements the real apply would run) to cfg.Out. On real
// apply it walks the plan in order, calls Applier.Apply for each.
// A mid-list failure aborts loud; the consuming service's DB-side
// `wc_migrations` table (D27, written by the migration's own
// `up_sql`) reflects the partial-success state on next deploy.
//
// Run does NOT update the lock — lock is read-only at apply time
// (D-iter3-7). Run does NOT contact console — apply is offline
// (D-iter3-6); audit / RecordApplied happen out-of-band.
func Run(ctx context.Context, cfg Config) error {
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}

	pending, err := Plan(ctx, cfg)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintln(out, "apply: nothing pending")
		return nil
	}

	if cfg.DryRun {
		fmt.Fprintf(out, "apply: dry-run — %d migration(s) would apply:\n", len(pending))
		for _, p := range pending {
			fmt.Fprintf(out, "\n--- %s :: %s ---\n", p.Connection, p.Migration.GetId())
			fmt.Fprintln(out, p.Migration.GetUpSql())
			// up_post_tx carries the non-transactional skirt (e.g.
			// CREATE INDEX CONCURRENTLY) the real apply also runs —
			// print it so the dry-run audit is complete, clearly
			// labelled as the separate post-tx phase.
			if pt := p.Migration.GetUpPostTx(); pt != "" {
				fmt.Fprintln(out, "-- up_post_tx (non-transactional) --")
				fmt.Fprintln(out, pt)
			}
		}
		return nil
	}

	// Cache one applier per connection (Plan returns connection-grouped,
	// so sibling migrations reuse the driver) and take a run-lock on each
	// non-transactional target; closeAll releases the locks then closes
	// the drivers.
	ac := newRunApplierCache(cfg.ApplierFor, out)
	defer ac.closeAll()

	logger := newLogger(out, cfg.LogFormat)
	for _, p := range pending {
		fmt.Fprintf(out, "apply: %s :: %s\n", p.Connection, p.Migration.GetId())

		applier, err := ac.get(ctx, p.Connection)
		if err != nil {
			return err
		}

		// No client-side signature verify: migrations are fetched
		// pre-verified from the console (public-split — the client holds
		// no verifier key); loadConnectionMigrations already did the
		// keyless content_sha256 integrity check.

		started := time.Now()
		if p.Adopt {
			// This database already sits at a migration the baseline
			// replaces, so the schema it describes is the schema in front
			// of us: record it, run none of its DDL. See Pending.Adopt.
			//
			// The bookkeeping body comes from the SERVER (adopt_sql),
			// which is where the dialect renderers live — the client
			// stays a dumb executor and gains no per-dialect knowledge.
			// It is run through the ordinary Apply so it inherits the
			// same transaction envelope, error reporting and run-lock as
			// everything else on this connection.
			adoptSQL := p.Migration.GetAdoptSql()
			if adoptSQL == "" {
				return fmt.Errorf("apply %s/%s: this migration supersedes %d migration(s) this database already applied, so its DDL must not run — but it carries no adopt_sql to record it with; the console that stored it is older than the squash support and cannot be adopted against safely",
					p.Connection, p.Migration.GetId(), len(p.Migration.GetSupersedes()))
			}
			adoptOnly := &applyfetchpb.Migration{
				Id:            p.Migration.GetId(),
				Connection:    p.Migration.GetConnection(),
				UpSql:         adoptSQL,
				ContentSha256: p.Migration.GetContentSha256(),
			}
			if err := applier.Apply(ctx, adoptOnly); err != nil {
				dur := time.Since(started)
				err = fmt.Errorf("adopt %s/%s: %w", p.Connection, p.Migration.GetId(), err)
				logMigration(logger, "adopt", p.Connection, p.Migration, dur, err)
				captureMigrationError("adopt", p.Connection, p.Migration, err)
				return err
			}
			// "already applied" is now a claim the plan has PROVEN: an
			// adopt is only reached when the head is the last id the
			// baseline collapsed. It was not before (T2-5 pass #15,
			// T25-A15-5) — a mid-range head adopted too, and this line told
			// the operator it had applied all N when it had applied a
			// prefix, so the one place the event is visible actively
			// misinformed.
			fmt.Fprintf(out, "apply: %s :: %s adopted (supersedes %d migration(s) through %s, all applied here; no DDL run)\n",
				p.Connection, p.Migration.GetId(), len(p.Migration.GetSupersedes()), lastSuperseded(p.Migration))
			logMigration(logger, "adopt", p.Connection, p.Migration, time.Since(started), nil)
			continue
		}
		if err := applyOrResume(ctx, applier, p.Migration, out); err != nil {
			dur := time.Since(started)
			err = fmt.Errorf("apply %s/%s: %w", p.Connection, p.Migration.GetId(), err)
			logMigration(logger, "apply", p.Connection, p.Migration, dur, err)
			captureMigrationError("apply", p.Connection, p.Migration, err)
			return err
		}
		logMigration(logger, "apply", p.Connection, p.Migration, time.Since(started), nil)
	}
	fmt.Fprintf(out, "apply: %d migration(s) applied\n", len(pending))
	return nil
}

// lastSuperseded is the newest migration a baseline collapsed — the one state a
// database must be at for the baseline to describe it.
//
// The list is stamped in history order (the freeze captures it that way), so
// the last element is the range's end. Compared by id rather than by position
// so a re-ordered list cannot quietly change the answer: ids are assigned
// monotonically per connection and the chain requires them to increase, which
// is the same property the plan's ordering already rests on.
func lastSuperseded(m *applyfetchpb.Migration) string {
	last := ""
	for _, s := range m.GetSupersedes() {
		if s > last {
			last = s
		}
	}
	return last
}

// supersedes reports whether m is a squash baseline that replaces the given
// migration id. Linear over a list that holds one project's collapsed history
// — bounded by how much history someone squashed, walked once per pending
// migration, so a map would optimise nothing worth the extra state.
func supersedes(m *applyfetchpb.Migration, id string) bool {
	for _, s := range m.GetSupersedes() {
		if s == id {
			return true
		}
	}
	return false
}

// applyOrResume applies a migration, resuming the post-tx phase when
// the applier reports it as PhasePending (Q52 two-phase crash
// recovery). For a ResumableApplier the orchestrator reads
// MigrationPhase first:
//
//   - PhasePending — a prior deploy committed the in-tx half (the
//     pending wc_migrations row exists) but crashed before the post-tx
//     skirt completed. Run ONLY the post-tx half; re-running the
//     committed in-tx DDL would wedge ("relation already exists").
//   - PhaseFresh / PhaseComplete — full Apply. (A pending-from-Plan
//     migration is never Complete: AppliedHead's post_tx_complete
//     filter keeps complete rows at/under the head cutoff.)
//
// Appliers without the capability (every non-PG dialect, plus the
// stub when it doesn't opt in) always take the plain Apply path.
func applyOrResume(ctx context.Context, applier Applier, m *applyfetchpb.Migration, out io.Writer) error {
	ra, ok := applier.(ResumableApplier)
	if !ok {
		return applier.Apply(ctx, m)
	}
	phase, err := ra.MigrationPhase(ctx, m.GetId())
	if err != nil {
		return fmt.Errorf("phase check: %w", err)
	}
	if phase == PhasePending {
		fmt.Fprintf(out, "apply:   resuming post-tx phase for %s (in-tx half already committed)\n", m.GetId())
		return ra.ApplyPostTx(ctx, m)
	}
	return applier.Apply(ctx, m)
}

// runApplierCache caches one Applier per connection for the duration of a
// Run / RunRollback loop and, for non-transactional stores (Redis / S3 that
// implement RunLockCapable), acquires a run-lock on first open so a
// non-idempotent TRANSFORM_FIELD data migration can't double-apply across
// concurrent runs. Transactional SQL dialects don't implement RunLockCapable
// (their wc_migrations PK serialises) and take the plain cached-applier path.
type runApplierCache struct {
	applierFor ApplierFor
	out        io.Writer
	cache      map[string]Applier
	// Q48-datamigrate-1: run-locks taken per non-transactional target, held
	// for the whole run, released by closeAll BEFORE Close so the lock's
	// final conditional write still has a live client.
	locks map[string]RunLock
}

func newRunApplierCache(applierFor ApplierFor, out io.Writer) *runApplierCache {
	return &runApplierCache{
		applierFor: applierFor,
		out:        out,
		cache:      map[string]Applier{},
		locks:      map[string]RunLock{},
	}
}

// get returns the cached applier for conn, opening (and run-locking, when the
// driver is RunLockCapable) one on first request.
func (c *runApplierCache) get(ctx context.Context, conn string) (Applier, error) {
	if a, ok := c.cache[conn]; ok {
		return a, nil
	}
	a, err := c.applierFor(conn)
	if err != nil {
		return nil, fmt.Errorf("connection %s: %w", conn, err)
	}
	if lc, ok := a.(RunLockCapable); ok {
		l, lerr := lc.AcquireRunLock(ctx)
		if lerr != nil {
			_ = a.Close()
			if errors.Is(lerr, ErrLockHeld) {
				return nil, fmt.Errorf("connection %s: another migration run is in progress against this target (run-lock held) — aborting", conn)
			}
			return nil, fmt.Errorf("connection %s: acquire run-lock: %w", conn, lerr)
		}
		c.locks[conn] = l
	}
	c.cache[conn] = a
	return a, nil
}

// closeAll releases every held run-lock (before closing, so the lock's final
// conditional write still has a live client) then closes every cached applier.
func (c *runApplierCache) closeAll() {
	for conn, l := range c.locks {
		if err := l.Release(context.Background()); err != nil {
			fmt.Fprintf(c.out, "migrate: warning: releasing run-lock for %s: %v\n", conn, err)
		}
	}
	for _, a := range c.cache {
		_ = a.Close()
	}
}

// logMigration emits one structured line per migration. Status =
// "ok" on success, "error" on failure (with the error message
// in the `error` attr). Duration is in milliseconds for easy
// telemetry consumption. Connection / dialect / migration_id
// always present so log-aggregation queries don't need to parse
// the human-readable msg.
func logMigration(logger *slog.Logger, action, connection string, m *applyfetchpb.Migration, dur time.Duration, err error) {
	status := "ok"
	attrs := []any{
		slog.String("action", action),
		slog.String("connection", connection),
		slog.String("migration_id", m.GetId()),
		slog.Int64("duration_ms", dur.Milliseconds()),
	}
	if err != nil {
		status = "error"
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	attrs = append(attrs, slog.String("status", status))
	logger.Info(action, attrs...)
}

// PlanRollback walks every connection in the lock, opens each
// connection's Applier to query AppliedHead (DB-side cutoff),
// loads filesystem migrations, and filters
// to-rollback = filesystem ∩ (id > ToMigrationID, id ≤ AppliedHead).
// Returned in REVERSE id order — the rollback Run loop walks
// newest-applied → oldest, stopping at ToMigrationID + 1.
//
// Empty ToMigrationID means "roll back everything currently
// applied" (= every id ≤ AppliedHead).
//
// An empty AppliedHead is NOT "nothing to roll back" (T2-5 pass #14, D14-2):
// the connection is skipped only when the head is empty AND no migration
// above it is half-applied. A PhasePending row on a fresh-looking database is
// exactly the state that has to be undone, and the fix that added it is the
// reason this sentence stopped being true.
func PlanRollback(ctx context.Context, cfg RollbackConfig) ([]Pending, error) {
	if cfg.MigrationsDir == "" {
		return nil, fmt.Errorf("migrate.PlanRollback: MigrationsDir is empty")
	}
	if cfg.ApplierFor == nil {
		return nil, fmt.Errorf("migrate.PlanRollback: ApplierFor is nil")
	}

	targets := append([]ConnTarget(nil), cfg.Targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].Connection < targets[j].Connection })

	var out []Pending
	for _, ct := range targets {
		diskMigs, err := loadConnectionMigrations(cfg.MigrationsDir, ct.Connection)
		if err != nil {
			return nil, fmt.Errorf("connection %s: %w", ct.Connection, err)
		}
		// ROLLBACK-UNPINNED — a connection declared in the lock but never pushed
		// has no migrations on disk (the w17ctl DSN resolver skips it too), so
		// there is nothing to roll back. Skip BEFORE opening an applier, otherwise
		// ApplierFor fails "no --target configured" and aborts the WHOLE rollback
		// on a declared-but-unpushed connection. (An empty ct.TargetMigrationID is
		// NORMAL for rollback — the target is cfg.ToMigrationID — so the presence of
		// on-disk migrations, not the pin, is the right signal.)
		if len(diskMigs) == 0 {
			continue
		}

		// ROLLBACK-NO-PIN — rollback is destructive and must be authenticated too;
		// verify the lock's target pin against the on-disk artifact, mirroring Plan.
		// (loadConnectionMigrations already re-checks each artifact's own
		// content_sha256; this anchors the target against the signed lock.)
		//
		// T25-D2-1: an empty TargetMigrationID is normal here (the rollback target
		// is cfg.ToMigrationID, not the lock pin), but a target that IS pinned must
		// carry its hash — a pinned-target-with-empty-hash silently skipped the
		// verification below, failing open exactly as Plan did. Refuse it.
		if ct.TargetMigrationID != "" && ct.TargetContentSha256 == "" {
			return nil, fmt.Errorf("connection %s: target_migration_id %q is pinned but target_content_sha256 is empty — refusing rollback (the content-integrity check cannot run; regenerate the lock)",
				ct.Connection, ct.TargetMigrationID)
		}
		if want := ct.TargetContentSha256; want != "" {
			var found *applyfetchpb.Migration
			for _, m := range diskMigs {
				if m.GetId() == ct.TargetMigrationID {
					found = m
					break
				}
			}
			if found == nil {
				return nil, fmt.Errorf("connection %s: target_migration_id %q not found in %s — run `migrate fetch` first",
					ct.Connection, ct.TargetMigrationID, filepath.Join(cfg.MigrationsDir, ct.Connection))
			}
			if want != found.GetContentSha256() {
				return nil, fmt.Errorf("connection %s: lock target_content_sha256=%s ≠ artifact %s for migration %q (someone hand-edited; refusing rollback)",
					ct.Connection, want, found.GetContentSha256(), ct.TargetMigrationID)
			}
		}

		applier, err := cfg.ApplierFor(ct.Connection)
		if err != nil {
			return nil, fmt.Errorf("connection %s: applier: %w", ct.Connection, err)
		}
		head, err := applier.AppliedHead(ctx)
		if err != nil {
			_ = applier.Close()
			return nil, fmt.Errorf("connection %s: AppliedHead: %w", ct.Connection, err)
		}
		// writer-F1 — AppliedHead deliberately EXCLUDES a PhasePending
		// (half-applied: in-tx half committed, skirt crashed) migration so Apply
		// can resume it. But rollback must ALSO undo such a migration's committed
		// in-tx DDL and clear its lying "pending" row — otherwise it sits ABOVE
		// the rollback ceiling, invisible to rollback, and a later re-apply
		// resumes only its post-tx half onto a schema its in-tx half assumed,
		// producing a falsely-"complete" ledger row for DDL that never ran. Probe
		// the migrations above `head` for PhasePending and roll those back too.
		pendingAbove := map[string]bool{}
		if res, ok := applier.(ResumableApplier); ok {
			for _, m := range diskMigs {
				if m.GetId() <= cfg.ToMigrationID {
					continue
				}
				if head != "" && m.GetId() <= head {
					continue // at/under head — handled by the complete-path filter below
				}
				ph, phErr := res.MigrationPhase(ctx, m.GetId())
				if phErr != nil {
					_ = applier.Close()
					return nil, fmt.Errorf("connection %s: MigrationPhase %s: %w", ct.Connection, m.GetId(), phErr)
				}
				if ph == PhasePending {
					pendingAbove[m.GetId()] = true
				}
			}
		}
		if closeErr := applier.Close(); closeErr != nil {
			return nil, fmt.Errorf("connection %s: Close after AppliedHead: %w", ct.Connection, closeErr)
		}

		if head == "" && len(pendingAbove) == 0 {
			// Fresh DB, no half-applied migration → nothing to roll back.
			continue
		}

		// Rollback selects from the CHAIN, exactly as apply does (T2-5 pass
		// #12). Before this it selected from whatever the directory held, which
		// made the destructive half of the client the one that would execute an
		// artifact nobody vouched for: measured, an inserted off-chain file that
		// forward apply REFUSES had its down_sql run on rollback. Same file,
		// same directory, opposite verdict — the B11-1 fix reached apply only.
		//
		// A rollback with no pinned target has no anchor to walk from, so the
		// set stays the directory. What that branch is NOT is the
		// fresh-service case — it said so until T2-5 pass #14 (A14-1) and the
		// claim was false in its own control flow: the only skip above is
		// `head == "" && no pending`, so this line is reached precisely when
		// migrations ARE applied. Measured at this seam, an unpinned
		// connection with `head="ts-2"` planned an off-chain `ts-15` and would
		// have run its down_sql.
		//
		// Where the safety actually comes from, stated so the next reader does
		// not have to re-derive it: w17ctl's `resolveDSNs` skips every
		// connection with no pin, so `ApplierFor` fails for one and the whole
		// rollback aborts at `applier` above — before AppliedHead, before any
		// selection. No operator can reach this branch through the product,
		// which is why A14-1's security half was refuted after being measured
		// through the real `RollbackCmd.Run`.
		//
		// ⚠️ That invariant lives in ANOTHER MODULE. `PlanRollback` is public
		// SDK, and a second caller wiring its own `ApplierFor` without the
		// pin-skip convention inherits the full off-chain-down_sql exposure.
		// Refusing here would move the invariant into this function — and it
		// is deliberately NOT done, because
		// `TestPlanRollback_NoPinnedTarget_StillWorks` records the opposite
		// decision: an unpinned rollback is normal, its target comes from
		// `ToMigrationID` rather than the lock pin. Reversing that is an owner
		// call, not an audit fix.
		rollbackSet := diskMigs
		if ct.TargetMigrationID != "" && ct.TargetContentSha256 != "" {
			var pinned *applyfetchpb.Migration
			for _, m := range diskMigs {
				if m.GetId() == ct.TargetMigrationID {
					pinned = m
					break
				}
			}
			if pinned != nil {
				chain, chainErr := chainFromTarget(diskMigs, pinned)
				if chainErr != nil {
					return nil, fmt.Errorf("connection %s: %w", ct.Connection, chainErr)
				}
				rollbackSet = chain
			}
		}
		onChain := make(map[string]bool, len(rollbackSet))
		for _, m := range rollbackSet {
			onChain[m.GetId()] = true
		}

		// Filter: id > ToMigrationID AND (id ≤ head OR half-applied above head).
		// Then reverse to walk newest-applied → oldest (pending-above sorts first).
		var toRollback []*applyfetchpb.Migration
		for _, m := range diskMigs {
			if m.GetId() <= cfg.ToMigrationID {
				continue
			}
			inHead := head != "" && m.GetId() <= head
			if !inHead && !pendingAbove[m.GetId()] {
				continue
			}
			// REFUSE rather than skip. An artifact inside the rollback range
			// that the chain does not vouch for is either tampered, or applied
			// history the lock's pin cannot reach (a pin lowered after a
			// deploy). Silently skipping it would report a completed rollback
			// while leaving its DDL in place, which is the worse of the two
			// failures a rollback can produce.
			if !onChain[m.GetId()] {
				return nil, fmt.Errorf("connection %s: migration %s is in the rollback range but is not on the chain the lock pins — refusing rollback (re-run `migrate fetch`; if the lock's target was lowered after this was applied, raise it back to cover the range being rolled back)",
					ct.Connection, m.GetId())
			}
			toRollback = append(toRollback, m)
		}
		// loadConnectionMigrations sorts ascending; reverse for
		// rollback order.
		for i, j := 0, len(toRollback)-1; i < j; i, j = i+1, j-1 {
			toRollback[i], toRollback[j] = toRollback[j], toRollback[i]
		}
		for _, m := range toRollback {
			out = append(out, Pending{Connection: ct.Connection, Migration: m})
		}
	}
	return out, nil
}

// RunRollback plans + rolls back. Inverse-apply equivalent of
// Run: walks pending in REVERSE id order, calls
// Applier.Rollback for each.
//
// No signature is verified here, and none can be: this module holds no
// verifier key by design (D4 — the offline client does zero crypto). What
// authenticates a body on this path is the CHAIN — `loadConnectionMigrations`
// recomputes each artifact's `content_sha256`, `PlanRollback` selects only
// from the chain the lock pins, and it REFUSES a row in range that the chain
// does not vouch for. The ed25519 verify happens SERVER-side, when the console
// serves the fetch.
//
// This doc used to say "Phase D signature verification runs on every body
// before the destructive op" (T2-5 pass #14, D14-1). Two other comments in
// this module said the same, while two more — twenty lines from one of them —
// correctly said "No client-side signature verify". The belief that a tampered
// artifact cannot execute here is the exact belief B11-1 lived under.
//
// Mid-list failure aborts loud; the DB-side `wc_migrations`
// reflects the partial-rollback state on next deploy. Lock is
// read-only at rollback time (matches Apply posture).
func RunRollback(ctx context.Context, cfg RollbackConfig) error {
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}

	pending, err := PlanRollback(ctx, cfg)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintln(out, "rollback: nothing to roll back")
		return nil
	}

	if cfg.DryRun {
		fmt.Fprintf(out, "rollback: dry-run — %d migration(s) would roll back:\n", len(pending))
		for _, p := range pending {
			fmt.Fprintf(out, "\n--- %s :: %s ---\n", p.Connection, p.Migration.GetId())
			// Print in execution order: down_pre_tx (the
			// non-transactional skirt, e.g. DROP INDEX CONCURRENTLY)
			// runs first, then down_sql. Both are surfaced so the
			// operator's audit matches what Applier.Rollback streams
			// — a rollback whose body lives entirely in down_pre_tx
			// would otherwise print nothing.
			if pre := p.Migration.GetDownPreTx(); pre != "" {
				fmt.Fprintln(out, "-- down_pre_tx (non-transactional) --")
				fmt.Fprintln(out, pre)
			}
			fmt.Fprintln(out, p.Migration.GetDownSql())
		}
		return nil
	}

	ac := newRunApplierCache(cfg.ApplierFor, out)
	defer ac.closeAll()

	logger := newLogger(out, cfg.LogFormat)
	for _, p := range pending {
		fmt.Fprintf(out, "rollback: %s :: %s\n", p.Connection, p.Migration.GetId())

		applier, err := ac.get(ctx, p.Connection)
		if err != nil {
			return err
		}

		// No client-side signature verify: the fetched migrations were
		// verified server-side by the console (public-split — the client holds no
		// verifier key), including the DOWN direction (fetch.go verifies both up and
		// down ed25519 signatures at fetch time). The client-side keyless
		// content_sha256 check (loadConnectionMigrations) anchors ALL four segments
		// — up_sql, up_post_tx, down_pre_tx, down_sql — via migrate.ContentHash
		// (writer-F2/sign-F5, landed), so the down body executed here is anchored too.

		started := time.Now()
		if err := applier.Rollback(ctx, p.Migration); err != nil {
			dur := time.Since(started)
			err = fmt.Errorf("rollback %s/%s: %w", p.Connection, p.Migration.GetId(), err)
			logMigration(logger, "rollback", p.Connection, p.Migration, dur, err)
			captureMigrationError("rollback", p.Connection, p.Migration, err)
			return err
		}
		logMigration(logger, "rollback", p.Connection, p.Migration, time.Since(started), nil)
	}
	fmt.Fprintf(out, "rollback: %d migration(s) rolled back\n", len(pending))
	return nil
}

// newLogger builds a *slog.Logger that writes one structured
// line per migration. format == "json" uses the JSON handler;
// any other value (including "" / "text") uses the text handler.
// Both write to `w` (= the orchestrator's cfg.Out so tests can
// capture into a buffer).
//
// The text handler suppresses the default "time" + "level" +
// "msg" prefix so the output stays readable next to the existing
// human-friendly Fprintf lines (`apply: foo :: ts-1`).
func newLogger(w io.Writer, format string) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	// Text handler with replace-attr that drops the slog prefix
	// so the per-migration line reads as `service=foo conn=main
	// id=ts-1 ...` rather than `2026-04-29T...Z INFO apply
	// service=foo ...`. Operators reading tail logs care about
	// the migration shape, not slog metadata.
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey, slog.LevelKey, slog.MessageKey:
				return slog.Attr{}
			}
			return a
		},
	}))
}

// captureMigrationError sends a per-migration apply / rollback
// failure to Sentry with the migration's id + connection +
// dialect tagged on the event so triage filters work
// out-of-the-box. No-op when Sentry isn't initialised
// (sentryx.Init with empty DSN; sentry.CaptureException returns
// without raising).
func captureMigrationError(action, connection string, m *applyfetchpb.Migration, err error) {
	hub := sentry.CurrentHub()
	if hub.Client() == nil {
		return // Sentry not initialised; skip without paying any cost.
	}
	hub.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("action", action)
		scope.SetTag("connection", connection)
		scope.SetTag("migration_id", m.GetId())
		hub.CaptureException(err)
	})
}

// chainFromTarget returns the migrations that lead to target, oldest→newest,
// by walking BACK from it through prev_content_sha256 (T2-5 B11-1).
//
// This is the reader half of the chain. The writer half — the predecessor's
// hash being an INPUT to each migration's own content_sha256, see
// migrate.ContentHash — is what makes the walk mean anything: because the
// signed lock pins the target's hash, and the target's hash covers its
// predecessor's, and so on, every migration the walk reaches is anchored to
// the lock. A file the walk does not reach is not vouched for by anything and
// must not run, however plausible its id looks.
//
// It refuses rather than guesses in three places, all of which are states no
// legitimate fetch produces:
//
//   - a broken link (prev names a hash no artifact carries) — the walk cannot
//     prove the rest of the history, and the half it CAN prove is not the set
//     the operator asked to apply;
//   - two artifacts carrying the same content hash — the link would be
//     ambiguous, and picking either one is picking on the attacker's behalf;
//   - a cycle, which cannot arise from an append-only console history and so
//     means the directory was written by something else. (The bound is the
//     artifact count, so a cycle terminates instead of hanging.)
//
// The unchained case is handled by the caller, which can tell a legitimate
// chain root from an old artifact set — see the refusal there.
func chainFromTarget(diskMigs []*applyfetchpb.Migration, target *applyfetchpb.Migration) ([]*applyfetchpb.Migration, error) {
	byHash := make(map[string]*applyfetchpb.Migration, len(diskMigs))
	for _, m := range diskMigs {
		h := m.GetContentSha256()
		if h == "" {
			continue
		}
		if prev, dup := byHash[h]; dup {
			return nil, fmt.Errorf("migrations %s and %s carry the same content_sha256 %s — the chain link to them is ambiguous, refusing apply",
				prev.GetId(), m.GetId(), h)
		}
		byHash[h] = m
	}

	var rev []*applyfetchpb.Migration
	seen := make(map[string]bool, len(diskMigs))
	for cur := target; cur != nil; {
		if seen[cur.GetContentSha256()] {
			return nil, fmt.Errorf("migration chain revisits %s — the artifact directory is not an append-only history, refusing apply", cur.GetId())
		}
		seen[cur.GetContentSha256()] = true
		rev = append(rev, cur)

		prev := cur.GetPrevContentSha256()
		if prev == "" {
			break // chain root: the first migration, or a squash baseline
		}
		next, ok := byHash[prev]
		if !ok {
			return nil, fmt.Errorf("migration %s chains to predecessor %s, which is not in the fetched set — refusing apply (run `migrate fetch` again; a partial history cannot be verified against the signed lock)",
				cur.GetId(), prev)
		}
		cur = next
	}

	slices.Reverse(rev)

	// Ids must INCREASE along the chain (T2-5 pass #12). The console assigns
	// them monotonically per connection, so this is a property of every
	// legitimate history — and it is what binds the id, which is deliberately
	// not an input to ContentHash (see that function's doc for why).
	//
	// The attack it closes: relabel an intermediate's id to sort at or below
	// the database's applied head. The body, digest and chain link all stay
	// valid, so the walk still reaches the migration — and the pending filter,
	// which cuts on id, then silently drops it. Measured: a tenant-isolation
	// constraint vanished from the plan while the deploy reported success.
	// Deleting the file instead would trip the missing-predecessor refusal;
	// relabelling was the evasion that did not.
	for i := 1; i < len(rev); i++ {
		if rev[i].GetId() <= rev[i-1].GetId() {
			return nil, fmt.Errorf("migration %s follows %s in the chain but its id does not sort after it — ids are assigned in order, so this artifact was relabelled; refusing apply",
				rev[i].GetId(), rev[i-1].GetId())
		}
	}
	return rev, nil
}

// unchainedTargetRefusal reports the state that cannot be resolved offline: a
// target that roots its own chain while OTHER migrations sit pending below it.
//
// A chain root is legitimate — the first migration on a connection, or a
// squash baseline, which replaces everything behind it and is meant to be the
// only thing applied. An artifact set produced before migrations were chained
// looks identical from here: every prev is empty, so the walk stops at the
// target and silently drops the intermediates the operator expects to run.
//
// Applying the range anyway is the defect this whole change removes, and it
// would be reachable by simply stripping prev from the files — so the fallback
// cannot exist. Refusing is safe in the other direction too: the target's own
// prev is covered by the hash the signed lock pins, so an attacker cannot
// manufacture this state, only an outdated fetch can.
func unchainedTargetRefusal(conn string, target *applyfetchpb.Migration, pendingBelow int) error {
	return fmt.Errorf("connection %s: target %s carries no predecessor link but %d earlier migration(s) are pending — these artifacts predate migration chaining, and an unchained set cannot be verified against the signed lock (the lock pins only the target). Run `migrate fetch` again to re-download them",
		conn, target.GetId(), pendingBelow)
}

// loadConnectionMigrations scans <root>/<conn>/ for *.json files,
// decodes each as protojson(Migration), verifies migrate.ContentHash over all
// four segments matches content_sha256, and returns them sorted by id (lex).
//
// Missing connection directory returns an empty list (legitimate
// state for a fresh service that hasn't fetched yet — apply
// produces a clear "no target pinned" message at a higher level).
func loadConnectionMigrations(root, connection string) ([]*applyfetchpb.Migration, error) {
	dir := filepath.Join(root, connection)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var out []*applyfetchpb.Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		buf, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		m := &applyfetchpb.Migration{}
		if err := protojson.Unmarshal(buf, m); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if got := ContentHash(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql()); got != m.GetContentSha256() {
			return nil, fmt.Errorf("artifact %s: content_sha256 mismatch (want %s, got %s — someone hand-edited)",
				path, m.GetContentSha256(), got)
		}
		// Backfill connection_name when missing (older fetches that
		// didn't stamp; harmless self-heal).
		if m.GetConnection() == "" {
			m.Connection = connection
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetId() < out[j].GetId() })
	return out, nil
}

// WriteMigration is the canonical fetch-side write — used by the
// fetch command to materialise one Migration on disk in the
// layout loadConnectionMigrations expects. Exposed as a public
// helper so tests + future tooling can produce the same artifact
// shape without duplicating the layout convention.
//
// Writes three files under <root>/<connection>/:
//
//	<id>.json    — canonical protojson(Migration); apply reads this
//	<id>.up.sql  — informational copy of up_sql for operator audit
//	<id>.down.sql — informational copy of down_sql for operator audit
//
// Verifies migrate.ContentHash(all four segments) == m.ContentSha256 before
// writing — catches a console-side mutation (or wire tampering) at the last
// opportunity before the artifact lands on disk.
func WriteMigration(root string, m *applyfetchpb.Migration) error {
	if m == nil {
		return fmt.Errorf("WriteMigration: nil migration")
	}
	if m.GetId() == "" {
		return fmt.Errorf("WriteMigration: empty id")
	}
	if m.GetConnection() == "" {
		return fmt.Errorf("WriteMigration: empty connection_name on %s", m.GetId())
	}
	if got := ContentHash(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql(), m.GetPrevContentSha256(), m.GetSupersedes(), m.GetAdoptSql()); got != m.GetContentSha256() {
		return fmt.Errorf("WriteMigration: content_sha256 mismatch on %s (registry=%s recomputed=%s)",
			m.GetId(), m.GetContentSha256(), got)
	}
	dir := filepath.Join(root, m.GetConnection())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	jsonBuf, err := protojson.MarshalOptions{UseProtoNames: true, Multiline: true, Indent: "  "}.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", m.GetId(), err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.GetId()+".json"), jsonBuf, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, m.GetId()+".up.sql"), []byte(m.GetUpSql()), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, m.GetId()+".down.sql"), []byte(m.GetDownSql()), 0o644); err != nil {
		return err
	}
	return nil
}

// RunAdopt brings an EXISTING database under migration management: it
// records every migration up to the pinned target as applied, without
// running any of their DDL, and refuses unless the database proves it
// already holds what those migrations introduce.
//
// # Why this is a separate entry point and not a flag on Run
//
// Adopting is the one operation whose whole purpose is to NOT do what the
// artifacts say. Reached by inference it would be a footgun — a plan that
// decided on its own to record rather than execute is a plan that can
// silently skip a real migration — so it is reached only by an operator
// naming it, and Run never chooses it.
//
// The squash adopt inside Run is a different thing wearing the same word.
// Its licence is a FACT the target reports (the applied head is the last id
// the baseline collapsed), so Run can decide it safely. An initial adopt has
// no such fact: an empty ledger says nothing about whether the schema is
// there, since a fresh database and a fully-provisioned one both report the
// same nothing.
//
// # What it refuses, and why each refusal is not conservatism
//
//   - A non-empty ledger. Then the database is already under management and
//     this is not an adoption; whatever the operator wanted, recording more
//     rows without running them is not it.
//   - A migration with no `adopt_preflight_sql`. Empty means the console
//     could not express it — a dialect that cannot ask, or a migration that
//     introduces no objects to ask about. Both leave the adopt resting on
//     nothing, which is the state this exists to replace.
//   - A failing preflight. That is the database saying it does not have
//     what the migration describes, which is the answer the hand-rolled
//     version of this never asked for.
//
// It records nothing until every preflight on the connection has passed, so
// a database that fails halfway is left un-adopted rather than
// half-adopted. (The preflight itself creates the ledger table — the one
// write it makes, and the one an adopt cannot avoid.)
func RunAdopt(ctx context.Context, cfg Config) error {
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	pending, err := Plan(ctx, cfg)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintln(out, "adopt: nothing to adopt — every migration up to the pinned target is already recorded")
		return nil
	}

	want := map[string]bool{}
	for _, c := range cfg.AdoptConnections {
		want[c] = true
	}

	byConn := map[string][]Pending{}
	var order []string
	for _, p := range pending {
		if len(want) > 0 && !want[p.Connection] {
			continue
		}
		if _, seen := byConn[p.Connection]; !seen {
			order = append(order, p.Connection)
		}
		byConn[p.Connection] = append(byConn[p.Connection], p)
	}
	if len(byConn) == 0 {
		return fmt.Errorf("adopt: no pending migration matches %v — the connections with something to adopt are the ones the lock pins and this database has not recorded", cfg.AdoptConnections)
	}

	// Every artifact is checked BEFORE any database is touched.
	//
	// This is the same rule the per-connection loop below follows — verify
	// everything, then record everything — lifted one level, because the
	// loop's version does not hold ACROSS connections. It did not, and the
	// first multi-connection project to try adoption proved it: a project
	// with postgres + redis recorded the postgres connection and only then
	// refused the redis one, leaving the project half-adopted in exactly
	// the state the inner split exists to prevent.
	//
	// It costs nothing to hoist: whether a migration carries the statements
	// an adopt needs is a property of the ARTIFACT, knowable with no
	// connection open at all.
	var unadoptable []string
	for _, conn := range order {
		for _, p := range byConn[conn] {
			if p.Migration.GetAdoptPreflightSql() == "" || p.Migration.GetAdoptSql() == "" {
				unadoptable = append(unadoptable, fmt.Sprintf("%s/%s", conn, p.Migration.GetId()))
			}
		}
	}
	if len(unadoptable) > 0 {
		return fmt.Errorf("adopt: refusing before writing anything — %d migration(s) cannot be adopted:\n  %s\n"+
			"A migration carries no adoption preflight when it introduces nothing a check can look for. That is "+
			"ordinary for a KV connection (a redis keyspace is created by writing a key, so the migration body is "+
			"comments) and for an ALTER-shaped migration (a column's presence is not a question a table check can "+
			"ask). Such a connection does not NEED adopting — its migration collides with nothing — so apply it "+
			"normally and narrow this command with --connection to the ones that do.",
			len(unadoptable), strings.Join(unadoptable, "\n  "))
	}

	cache := newRunApplierCache(cfg.ApplierFor, out)
	defer cache.closeAll()

	for _, conn := range order {
		applier, err := cache.get(ctx, conn)
		if err != nil {
			return err
		}
		// An adoption starts from an unmanaged database. A ledger with a
		// head is a database that has been applied to, and recording
		// migrations onto it without running them would punch a hole in a
		// history that was, until now, complete.
		head, err := applier.AppliedHead(ctx)
		if err != nil {
			return fmt.Errorf("adopt %s: read applied head: %w", conn, err)
		}
		if head != "" {
			return fmt.Errorf("adopt %s: refusing — this database is already under migration management (its ledger is at %s). Adopt brings an UNMANAGED database in; from here on the ordinary `migrate apply` is the way forward",
				conn, head)
		}

		ms := byConn[conn]
		// Every preflight first, then every record. Splitting the two
		// passes is what makes a partial failure leave nothing behind: a
		// database missing the third migration's tables must not come out
		// of this with the first two recorded, because that state is
		// neither adopted nor clean and no later command knows it happened.
		for _, p := range ms {
			// Non-empty by the hoisted check above; read without re-testing
			// so there is ONE place that decides what is adoptable.
			preflight := p.Migration.GetAdoptPreflightSql()
			probe := &applyfetchpb.Migration{
				Id:         p.Migration.GetId(),
				Connection: p.Migration.GetConnection(),
				UpSql:      preflight,
			}
			if err := applier.Apply(ctx, probe); err != nil {
				return fmt.Errorf("adopt %s/%s: this database does not hold what the migration describes: %w", conn, p.Migration.GetId(), err)
			}
		}

		for _, p := range ms {
			adoptSQL := p.Migration.GetAdoptSql()
			rec := &applyfetchpb.Migration{
				Id:            p.Migration.GetId(),
				Connection:    p.Migration.GetConnection(),
				UpSql:         adoptSQL,
				ContentSha256: p.Migration.GetContentSha256(),
			}
			if err := applier.Apply(ctx, rec); err != nil {
				return fmt.Errorf("adopt %s/%s: %w", conn, p.Migration.GetId(), err)
			}
			fmt.Fprintf(out, "adopt: %s :: %s recorded (verified present; no DDL run)\n", conn, p.Migration.GetId())
		}
	}
	return nil
}
