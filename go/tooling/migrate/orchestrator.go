package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
// loads filesystem migrations from MigrationsDir, and filters
// pending = filesystem ∩ (id > applied_head, id ≤ target). Hashes
// are recomputed from each artifact's up_sql body and verified
// against the .json file's content_sha256.
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

		// Filter pending. diskMigs is already sorted by id (load
		// guarantees this). head=="" → everything ≤ target;
		// otherwise strict-after head, up to + including target.
		for _, m := range diskMigs {
			if head != "" && m.GetId() <= head {
				continue
			}
			if m.GetId() > target {
				continue
			}
			out = append(out, Pending{Connection: ct.Connection, Migration: m})
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
// applied" (= every id ≤ AppliedHead). Empty AppliedHead =
// nothing to roll back; returns the empty slice.
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
// Applier.Rollback for each. Phase D signature verification
// runs on every body before the destructive op (rollback is
// destructive — must be authenticated).
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
		if got := ContentHash(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql()); got != m.GetContentSha256() {
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
	if got := ContentHash(m.GetUpSql(), m.GetUpPostTx(), m.GetDownPreTx(), m.GetDownSql()); got != m.GetContentSha256() {
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
