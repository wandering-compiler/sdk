// Package migratecli is the `migrate` subcommand a generated storage binary
// mounts, so the binary that OWNS a database is the thing that migrates it.
//
// Why it is not `w17ctl`'s job any more: applying a migration needs the
// database, and a production database is deliberately unreachable from
// anywhere but its own network — `infra/services/console/prod/compose.yaml`
// says it in as many words ("No published ports: reachable only on the
// overlay"). So the applier has to RUN where the database lives, and the
// thing already running there, already holding the DSNs, is the storage
// bundle. Everything else about the design follows from that one constraint.
//
// It carries no compiler knowledge and cannot: a migration body is native SQL
// the console lowered at generate time, and this package's whole job is to
// hand those bytes to a driver. The apply closure has ZERO private srcgo and
// zero DQL packages in it — the same closure `w17ctl` links today, mounted
// somewhere else.
package migratecli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// Options is what the generated binary knows and this package does not.
type Options struct {
	// Connections the calling bundle owns. A storage bundle migrates ITS
	// connections and no others — two bundles in one project each hold a
	// different slice of the lock, and a bundle reaching past its own would
	// apply DDL for a database it does not serve and cannot test against.
	//
	// Empty means every connection in the lock, which is what a single-bundle
	// project wants and what a test harness passes.
	Connections []string

	// Out is the progress sink. Defaults to os.Stdout.
	Out io.Writer

	// Getenv reads the DSN + console variables. Nil means os.Getenv, which is
	// what a deployed binary uses.
	//
	// It exists so a harness can drive the REAL entry point rather than a
	// reimplementation of it: the alternative is t.Setenv, which is process
	// global and rules out running two fixtures at once. A seam that lets the
	// migration suite exercise this code path instead of a copy of it is worth
	// one field.
	Getenv func(string) string
}

// getenv resolves the environment reader.
func (o Options) getenv() func(string) string {
	if o.Getenv != nil {
		return o.Getenv
	}
	return os.Getenv
}

// Main runs `migrate <subcommand>`; args are the tokens AFTER the `migrate`
// word. Returns an error the caller reports and exits non-zero on.
func Main(ctx context.Context, args []string, opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	if len(args) == 0 {
		return usageErr("")
	}
	switch args[0] {
	case "apply":
		return runApply(ctx, args[1:], opts, out)
	case "fetch":
		return runFetch(ctx, args[1:], opts, out)
	case "rollback":
		return runRollback(ctx, args[1:], opts, out)
	case "status":
		return runStatus(ctx, args[1:], opts, out)
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return nil
	default:
		return usageErr(args[0])
	}
}

const usage = `usage: <binary> migrate <command> [flags]

  apply    apply every pending migration up to each connection's pinned target
  fetch    download artefacts from the console into --migrations, apply nothing
  rollback run the DOWN bodies of everything applied above --to, in reverse
  status   report what is applied and what is pending, and change nothing

flags (apply):
  --lock PATH         lock file holding the per-connection targets (default w17/lock.yaml)
  --migrations DIR    directory of artefacts a fetch produced (default w17/migrations)
  --console ADDR      console endpoint; overrides W17_CONSOLE_ADDR
  --fetch             pull the set from the console and apply it WITHOUT writing
                      to disk; ignores --migrations. Needs W17_CONSOLE_ADDR +
                      W17_CONSOLE_TOKEN
  --dry-run           print what would apply, apply nothing
  --log-format text|json
  --parallel N        worker count for KV data migrations; 0 = the migration's own

flags (rollback):
  --to MIGRATION_ID   highest id to KEEP applied. REQUIRED and has no default:
                      rollback is destructive, and an omitted value that meant
                      "everything" would be one forgotten flag away from an
                      empty database. Pass --to="" to mean everything, and mean it.

Database DSNs are read from W17_TARGET_<CONNECTION> environment variables,
never from flags — a credential in a flag lands in shell history and in every
process listing on the host.

"apply --fetch" is the one-step form for a container with no writable disk.
"fetch" then "apply" stays available for an operator who wants to look at the
artefacts in between, and for a deployment that would rather not hold a
console credential at all.
`

func usageErr(got string) error {
	if got == "" {
		return fmt.Errorf("migrate: a subcommand is required (apply, fetch, rollback, status)\n\n%s", usage)
	}
	return fmt.Errorf("migrate: unknown subcommand %q (want apply, fetch, rollback, status)\n\n%s", got, usage)
}

// applyFlags is the flag set apply and status share; status ignores the ones
// that only mean something when something is actually run.
type applyFlags struct {
	lock       string
	migrations string
	console    string
	to         string
	toSet      bool
	fetch      bool
	dryRun     bool
	logFormat  string
	parallel   int
}

func parseFlags(name string, args []string, out io.Writer) (applyFlags, error) {
	var f applyFlags
	fs := flag.NewFlagSet("migrate "+name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&f.lock, "lock", "w17/lock.yaml", "lock file holding per-connection targets")
	fs.StringVar(&f.migrations, "migrations", "w17/migrations", "directory of fetched artefacts")
	fs.StringVar(&f.console, "console", "", "console endpoint; overrides W17_CONSOLE_ADDR")
	fs.StringVar(&f.to, "to", "", "rollback only: highest migration id to KEEP applied")
	fs.BoolVar(&f.fetch, "fetch", false, "pull artefacts from the console and apply in memory (no disk)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print pending migrations without applying")
	fs.StringVar(&f.logFormat, "log-format", "text", "per-migration log line format: text or json")
	fs.IntVar(&f.parallel, "parallel", 0, "worker count for KV data migrations; 0 = the migration's own")
	if err := fs.Parse(args); err != nil {
		return f, fmt.Errorf("migrate %s: %w", name, err)
	}
	if f.logFormat != "text" && f.logFormat != "json" {
		return f, fmt.Errorf("migrate %s: --log-format must be text or json, got %q", name, f.logFormat)
	}
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "to" {
			f.toSet = true
		}
	})
	return f, nil
}

func runApply(ctx context.Context, args []string, opts Options, out io.Writer) error {
	f, err := parseFlags("apply", args, out)
	if err != nil {
		return err
	}
	cfg, err := buildConfig(ctx, f, opts, out)
	if err != nil {
		return err
	}
	cfg.DryRun = f.dryRun
	cfg.LogFormat = f.logFormat
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return migrate.Run(ctx, cfg)
}

func runStatus(ctx context.Context, args []string, opts Options, out io.Writer) error {
	f, err := parseFlags("status", args, out)
	if err != nil {
		return err
	}
	cfg, err := buildConfig(ctx, f, opts, out)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	pending, err := migrate.Plan(ctx, cfg)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintln(out, "migrate status: up to date — nothing pending")
		return nil
	}
	fmt.Fprintf(out, "migrate status: %d migration(s) pending\n", len(pending))
	for _, p := range pending {
		fmt.Fprintf(out, "  %-24s %s\n", p.Connection, p.Migration.GetId())
	}
	return nil
}

// runRollback runs the down bodies of everything applied above --to, newest
// first.
//
// It lives here for the same reason apply does — it writes to the database, so
// it runs where the database is. It is if anything the stronger case: a
// rollback is the operation you reach for when something is already wrong, and
// needing a second tool installed somewhere with network reach to the database
// is exactly the wrong moment to discover it is missing.
func runRollback(ctx context.Context, args []string, opts Options, out io.Writer) error {
	f, err := parseFlags("rollback", args, out)
	if err != nil {
		return err
	}
	// --to is REQUIRED, and the empty string is a legal VALUE of it rather
	// than its absence. Defaulting an omitted --to to "" would make "roll back
	// everything" the behaviour of forgetting a flag.
	if !f.toSet {
		return fmt.Errorf(
			"migrate rollback: --to is required\n" +
				"  --to <id>  roll back everything applied ABOVE that id, keeping it\n" +
				"  --to \"\"    roll back everything currently applied\n" +
				"  why: no default. The value that means \"undo all of it\" must be typed,\n" +
				"       not arrived at by leaving a flag off a destructive command.")
	}
	cfg, err := buildConfig(ctx, f, opts, out)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return migrate.RunRollback(ctx, migrate.RollbackConfig{
		Targets:       cfg.Targets,
		MigrationsDir: cfg.MigrationsDir,
		MigrationsFS:  cfg.MigrationsFS,
		ApplierFor:    cfg.ApplierFor,
		Out:           out,
		DryRun:        f.dryRun,
		LogFormat:     f.logFormat,
		ToMigrationID: f.to,
	})
}

// buildConfig assembles the orchestrator config from the lock, the owned
// connection set, and the environment's DSNs.
func buildConfig(ctx context.Context, f applyFlags, opts Options, out io.Writer) (migrate.Config, error) {
	lk, err := migrate.LoadLockView(f.lock)
	if err != nil {
		return migrate.Config{}, fmt.Errorf("migrate: %w", err)
	}
	targets := ownedTargets(lk.ConnTargets(), opts.Connections)
	if len(targets) == 0 {
		// Not an error worth failing a deploy over on its own, but it must not
		// read as success either: a bundle that migrates nothing because it
		// matched no connection is a wiring mistake, and "nothing pending" is
		// exactly what it would otherwise print.
		return migrate.Config{}, fmt.Errorf(
			"migrate: none of this bundle's connections (%s) appear in %s\n"+
				"  why: the binary migrates the connections it serves; a name that is not in the lock\n"+
				"       is either a stale build or the wrong lock for this deployment",
			strings.Join(opts.Connections, ", "), f.lock,
		)
	}
	specs, err := dsnSpecs(targets, opts.getenv())
	if err != nil {
		return migrate.Config{}, err
	}
	cfg := migrate.Config{
		Targets:    targets,
		ApplierFor: factory.FromTargets(specs, factory.WithParallel(f.parallel)),
		Out:        out,
	}
	if !f.fetch {
		cfg.MigrationsDir = f.migrations
		return cfg, nil
	}
	// --fetch: the artefacts arrive over the wire and never land anywhere.
	// They still go through the loader, so the content-hash check that guards
	// the on-disk path guards this one identically.
	migs, err := fetchMigrations(ctx, lk.ProjectID, targets, consoleAddrOf(f.console, opts.getenv()))
	if err != nil {
		return migrate.Config{}, err
	}
	fmt.Fprintf(out, "migrate: fetched %d migration(s) from the console (in memory; nothing written)\n", len(migs))
	set, err := migrate.MigrationSetFS(migs)
	if err != nil {
		return migrate.Config{}, fmt.Errorf("migrate: %w", err)
	}
	cfg.MigrationsFS = set
	return cfg, nil
}

// runFetch downloads the artefacts and writes them, applying nothing. The
// separate step an operator uses when they want to read what is about to run.
func runFetch(ctx context.Context, args []string, opts Options, out io.Writer) error {
	f, err := parseFlags("fetch", args, out)
	if err != nil {
		return err
	}
	lk, err := migrate.LoadLockView(f.lock)
	if err != nil {
		return fmt.Errorf("migrate fetch: %w", err)
	}
	targets := ownedTargets(lk.ConnTargets(), opts.Connections)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	migs, err := fetchMigrations(ctx, lk.ProjectID, targets, consoleAddrOf(f.console, opts.getenv()))
	if err != nil {
		return err
	}
	for _, m := range migs {
		if err := migrate.WriteMigration(f.migrations, m); err != nil {
			return fmt.Errorf("migrate fetch: write %s/%s: %w", m.GetConnection(), m.GetId(), err)
		}
	}
	fmt.Fprintf(out, "migrate fetch: %d migration(s) written to %s\n", len(migs), f.migrations)
	return nil
}

// ownedTargets narrows the lock's connections to the ones this bundle serves.
// An empty owned set means "all", for a single-bundle project or a test.
func ownedTargets(all []migrate.ConnTarget, owned []string) []migrate.ConnTarget {
	if len(owned) == 0 {
		return all
	}
	want := make(map[string]struct{}, len(owned))
	for _, c := range owned {
		want[c] = struct{}{}
	}
	out := make([]migrate.ConnTarget, 0, len(owned))
	for _, t := range all {
		if _, ok := want[t.Connection]; ok {
			out = append(out, t)
		}
	}
	return out
}

// dsnSpecs reads each pinned connection's DSN from the environment.
//
// Unpinned connections are skipped rather than demanded: a connection with no
// target has never been schema-pushed, so there is nothing to apply and
// insisting on its DSN would block a deploy on a database nobody is migrating.
func dsnSpecs(targets []migrate.ConnTarget, getenv func(string) string) ([]factory.TargetSpec, error) {
	var specs []factory.TargetSpec
	var missing []string
	for _, t := range targets {
		if t.TargetMigrationID == "" {
			continue
		}
		dsn := getenv(migrate.TargetEnvVar(t.Connection))
		if dsn == "" {
			missing = append(missing, t.Connection)
			continue
		}
		specs = append(specs, factory.TargetSpec{Connection: t.Connection, DSN: dsn})
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for _, c := range missing {
			names = append(names, c+" → "+migrate.TargetEnvVar(c))
		}
		return nil, fmt.Errorf(
			"migrate: no DSN for connection(s): %s\n"+
				"  fix: set each variable to that connection's database URL",
			strings.Join(names, ", "),
		)
	}
	return specs, nil
}

// consoleAddrOf lets --console override the environment, without giving the
// TOKEN a flag too.
//
// The asymmetry is the point: an address is routing and belongs in a command
// line, a credential is not and does not. A --token flag would put the secret
// in shell history and in every process listing on the host, which is the same
// reason DSNs are env-only.
func consoleAddrOf(flagVal string, getenv func(string) string) func(string) string {
	if flagVal == "" {
		return getenv
	}
	return func(k string) string {
		if k == envConsoleAddr {
			return flagVal
		}
		return getenv(k)
	}
}
