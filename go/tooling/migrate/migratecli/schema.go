package migratecli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// The dev schema, applied by the process that owns the database.
//
// This is the DEV half of the split, and it is deliberately not a migration.
// A dev database is reconciled to what the models currently say — the planner
// diffs the schema against the store and applies the difference — so there is
// no series to walk and no ledger to keep. Production is the other half:
// there a reviewed, recorded migration series is the only thing that touches
// the database, which is what `migrate apply` is for.
//
// It exists because the thing it replaces ran at the wrong time. A `db/init`
// directory is executed by the postgres entrypoint ONLY on a FRESH volume, so
// a schema change never reached a database that already existed — the console
// hit that twice, as `invalid credentials` and a dead invite endpoint, from a
// column and a table its running database had never been given. A step in the
// stack runs every time the stack comes up.

// Schema runs `schema <subcommand>`; args are the tokens AFTER the `schema`
// word.
func Schema(ctx context.Context, args []string, opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	if len(args) == 0 {
		return fmt.Errorf("schema: a subcommand is required (apply)\n\n%s", schemaUsage)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(out, schemaUsage)
		return nil
	}
	return runSchema(ctx, args, opts, out)
}

const schemaUsage = `usage: <binary> schema apply [flags]

Build this bundle's DEV databases from a plan rendered at generate time.
Nothing is recorded, because a dev database is defined by the current schema
rather than by a history.

It BUILDS a schema and does not reconcile one: the plan is rendered from empty
state, at a moment when no database exists to diff against. A store that
already holds tables is left alone with a line saying so. To change an existing
dev database use "w17ctl stack build", which plans against the live store; to
change a production one use "<binary> migrate apply".

  --lock PATH     lock file naming this bundle's connections (default w17/lock.yaml)
  --schema DIR    directory holding the rendered plan (default w17/schema)
  --dry-run       print what would run, run nothing

Production is the other half of the split and is NOT this command: there the
recorded series is the only thing that touches a database. See
"<binary> migrate --help".

DSNs come from W17_TARGET_<CONNECTION>, never a flag.
`

// cloneForConnections keeps only the connections this bundle serves.
func cloneForConnections(plan *applyplanpb.DevApplyPlan, owned map[string]bool) *applyplanpb.DevApplyPlan {
	out := &applyplanpb.DevApplyPlan{}
	for _, m := range plan.GetMigrations() {
		if owned[m.GetConnection()] {
			out.Migrations = append(out.Migrations, m)
		}
	}
	return out
}

// join renders a diagnostic list.
func join(v []string) string { return strings.Join(v, ", ") }

// runSchema implements `<binary> schema apply`.
func runSchema(ctx context.Context, args []string, opts Options, out io.Writer) error {
	if args[0] != "apply" {
		return fmt.Errorf("schema: the only subcommand is `apply`\n\n%s", schemaUsage)
	}
	f, err := parseFlags("schema apply", args[1:], out)
	if err != nil {
		return err
	}
	lk, err := migrate.LoadLockView(f.lock)
	if err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	targets := ownedTargets(lk.ConnTargets(), opts.Connections)
	if len(targets) == 0 {
		return fmt.Errorf("schema: none of this bundle's connections appear in %s", f.lock)
	}
	plan, err := migrate.LoadDevPlan(os.DirFS(f.schema))
	if err != nil {
		return fmt.Errorf(
			"schema: read %s: %w\n"+
				"  why: the plan is rendered at generate time and executed here\n"+
				"  fix: run `w17ctl schema render` and commit what it writes",
			f.schema, err)
	}

	// A plan carries every connection the PROJECT has; this bundle owns some
	// of them. Narrowing here rather than letting the applier factory fail on
	// a name it has no DSN for: another bundle's database is not this
	// binary's to change, which is the same rule migrate apply follows.
	specs, withoutDSN := seedSpecs(targets, opts.getenv())
	owned := map[string]bool{}
	for _, s := range specs {
		owned[s.Connection] = true
	}
	var mine []int
	for i, m := range plan.GetMigrations() {
		if owned[m.GetConnection()] {
			mine = append(mine, i)
		}
	}
	if len(mine) == 0 {
		unset := ""
		if len(withoutDSN) > 0 {
			unset = "\n  no DSN set for: " + join(withoutDSN)
		}
		fmt.Fprintf(out, "schema: nothing for this bundle's connections (%s)%s\n",
			connectionNames(specs), unset)
		return nil
	}
	narrowed := plan
	if len(mine) != len(plan.GetMigrations()) {
		narrowed = cloneForConnections(plan, owned)
	}

	if f.dryRun {
		fmt.Fprintf(out, "schema: would apply %d connection plan(s)\n", len(mine))
		for _, m := range narrowed.GetMigrations() {
			fmt.Fprintf(out, "  %-24s %d byte(s) of DDL\n", m.GetConnection(), len(m.GetUpSql()))
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	applierFor := newApplierFor(specs)

	// A store that already holds a schema is LEFT ALONE, and that is a real
	// limit rather than caution.
	//
	// This plan was rendered from EMPTY state, because rendering happens at
	// generate time when no database exists to diff against. It builds a
	// schema; it cannot reconcile one. Run against a store that already has
	// the tables it creates, it fails on the first CREATE — so the honest
	// behaviour is to say so and name the two commands that DO change an
	// existing schema, rather than either crashing or pretending.
	todo := &applyplanpb.DevApplyPlan{}
	for _, m := range narrowed.GetMigrations() {
		populated, err := storeHasSchema(ctx, applierFor, m.GetConnection())
		if err != nil {
			return fmt.Errorf("schema: %s: %w", m.GetConnection(), err)
		}
		if populated {
			fmt.Fprintf(out,
				"schema: %s already has a schema — left alone\n"+
					"  why: this plan builds a schema from empty; it cannot reconcile one\n"+
					"  to CHANGE an existing dev database: w17ctl stack build (diff-apply)\n"+
					"  to change a PRODUCTION one: <binary> migrate apply\n",
				m.GetConnection())
			continue
		}
		todo.Migrations = append(todo.Migrations, m)
	}
	if len(todo.GetMigrations()) == 0 {
		return nil
	}
	if err := migrate.DevApply(ctx, todo, applierFor); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	for _, m := range todo.GetMigrations() {
		fmt.Fprintf(out, "schema: %s created\n", m.GetConnection())
	}
	return nil
}

// emptyFingerprint is what FingerprintCapable reports for a store holding no
// tables — computed rather than pinned, so it tracks the format it compares
// against.
var emptyFingerprint = fingerprint.Schema{}.FingerprintHex()

// storeHasSchema reports whether a connection's store already holds tables.
//
// A store that cannot answer reports FALSE, and the apply goes ahead. That is
// the right way round: a KV store has no fingerprint and is genuinely
// re-appliable (its plan is keyspace declarations), while refusing on "I
// cannot tell" would block the case this command exists for.
func storeHasSchema(ctx context.Context, applierFor migrate.ApplierFor, conn string) (bool, error) {
	ap, err := applierFor(conn)
	if err != nil {
		return false, err
	}
	defer func() { _ = ap.Close() }()
	fp, ok := ap.(migrate.FingerprintCapable)
	if !ok {
		return false, nil
	}
	got, err := fp.Fingerprint(ctx)
	if err != nil {
		return false, fmt.Errorf("read the store's schema state: %w", err)
	}
	return got != "" && got != emptyFingerprint, nil
}
