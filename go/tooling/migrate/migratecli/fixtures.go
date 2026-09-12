package migratecli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// Seeding, from the process that can reach the database.
//
// The e2e stack's Postgres publishes an ephemeral port and is seeded only from
// its initdb directory — `w17ctl fixtures apply --out` exists precisely because
// there was "no stable DSN to apply against" from outside. That is the same
// wall migrations hit, and it has the same answer: the binary that OWNS the
// database is inside the network, holds the DSN, and can simply do it.
//
// What it does NOT do is render. Turning a fixture into statements needs the
// schema and the fixtures package, both of which are compiler-domain and live
// in private srcgo. The console renders; this executes. Same split as
// migrations, and the same two ways to reach the rendered form: read what an
// earlier render wrote to disk (`--fixtures DIR`, the default), or render now
// over the wire (`--fetch`).

// newApplierFor opens the appliers a seeding run needs. A package var so a
// test can drive this command's real code path against a fake store rather
// than a reimplementation of it — the same reason Options.Getenv exists.
var newApplierFor = func(specs []factory.TargetSpec) migrate.ApplierFor {
	return factory.FromTargets(specs)
}

// runFixtures implements `<binary> fixtures apply`.
func runFixtures(ctx context.Context, args []string, opts Options, out io.Writer) error {
	if len(args) == 0 || args[0] != "apply" {
		return fmt.Errorf("fixtures: the only subcommand is `apply`\n\n%s", usage)
	}
	f, err := parseFlags("fixtures apply", args[1:], out)
	if err != nil {
		return err
	}
	lk, err := migrate.LoadLockView(f.lock)
	if err != nil {
		return fmt.Errorf("fixtures: %w", err)
	}
	targets := ownedTargets(lk.ConnTargets(), opts.Connections)
	if len(targets) == 0 {
		return fmt.Errorf("fixtures: none of this bundle's connections appear in %s", f.lock)
	}
	specs, withoutDSN := seedSpecs(targets, opts.getenv())

	seeds, err := loadSeeds(ctx, f, lk, opts, out)
	if err != nil {
		return err
	}
	if len(seeds) == 0 {
		// Say WHERE it looked. "Nothing to seed" is a success, and a success
		// that names no source cannot be told apart from a run pointed at the
		// wrong directory or filtered by a group nobody rendered — which is
		// exactly how it read to deinvo, who took it as proof the step had run
		// (2026-09-12). The three inputs that decide this answer are the
		// directory, the domain filter and the group, so all three are named.
		where := f.fixtures
		if f.fetch {
			where = "the console (--fetch)"
		}
		domain := f.domain
		if domain == "" {
			domain = "(every domain)"
		}
		fmt.Fprintf(out, "fixtures: nothing to seed in group %s\n  looked in: %s\n  domain:    %s\n"+
			"  note: this is a SUCCESS — no fixture matched. If you expected rows, check that a render step wrote them there.\n",
			groupLabel(f.group), where, domain)
		return nil
	}
	// There ARE rows to seed, so a run with no DSN anywhere cannot do its job.
	//
	// openSeedTarget below also refuses this case, but it refuses it as a
	// STORE-TYPE problem ("no connection a fixture can be seeded into", with a
	// why about KV stores having nothing to insert rows as) — true of a bundle
	// serving only Redis, and misleading for the far commoner cause of a
	// variable nobody exported. Two causes, one refusal, and the message
	// named the rarer one.
	if err := requireSomeDSN("fixtures", specs, withoutDSN, f.allowNoDSN); err != nil {
		return err
	}

	// The whole group is ONE unit of work. Fixtures reference each other
	// across files — a `tasks` fixture FKs the categories a `taxonomy` one
	// creates — and the renderer orders rows within a file, not between them.
	// Applying file by file would let a failure in the third one leave the
	// first two behind, which is a database in a state no fixture describes.
	var stmts []migrate.SeedStmt
	for _, s := range seeds {
		for _, st := range s.Statements {
			args := make([]any, 0, len(st.GetArgs()))
			for _, a := range st.GetArgs() {
				args = append(args, a.AsInterface())
			}
			stmts = append(stmts, migrate.SeedStmt{SQL: st.GetSql(), Args: args})
		}
	}

	if f.dryRun {
		fmt.Fprintf(out, "fixtures: would apply %d fixture(s), %d statement(s) in group %s\n",
			len(seeds), len(stmts), groupLabel(f.group))
		for _, s := range seeds {
			fmt.Fprintf(out, "  %s/%s (%d statement(s))\n", s.Domain, s.Name, len(s.Statements))
		}
		return nil
	}

	conn, seeder, closeSeeder, err := openSeedTarget(specs, withoutDSN, f.connection)
	if err != nil {
		return err
	}
	defer closeSeeder()

	if err := seeder.ExecSeed(ctx, stmts); err != nil {
		return fmt.Errorf("fixtures: seed %s: %w", conn, err)
	}
	fmt.Fprintf(out, "fixtures: group %s — %d fixture(s), %d statement(s) on %s\n",
		groupLabel(f.group), len(seeds), len(stmts), conn)
	return nil
}

// loadSeeds resolves the rendered fixtures, from disk or from the console.
func loadSeeds(ctx context.Context, f applyFlags, lk *migrate.LockView, opts Options, out io.Writer) ([]migrate.FixtureSeed, error) {
	if !f.fetch {
		fsys := os.DirFS(f.fixtures)
		seeds, err := migrate.LoadFixtureSeeds(fsys, f.domain, f.group)
		if err != nil {
			return nil, fmt.Errorf(
				"fixtures: read %s: %w\n"+
					"  why: without --fetch the seeds are read from disk, where an earlier render wrote them\n"+
					"  fix: run the render step that populates %s, or pass --fetch to render over the wire",
				f.fixtures, err, f.fixtures)
		}
		return seeds, nil
	}
	return fetchSeeds(ctx, f, lk, opts, out)
}

// fetchSeeds renders the group over the wire — the form for an environment
// that can reach the console and would rather not carry an artefact.
func fetchSeeds(ctx context.Context, f applyFlags, lk *migrate.LockView, opts Options, out io.Writer) ([]migrate.FixtureSeed, error) {
	getenv := consoleAddrOf(f.console, opts.getenv())
	addr := getenv(envConsoleAddr)
	if addr == "" {
		return nil, fmt.Errorf(
			"fixtures: --fetch needs a console address — it is the console that renders a fixture into statements\n"+
				"  fix: set %s to the console's gRPC endpoint, or drop --fetch to read %s from disk",
			envConsoleAddr, f.fixtures)
	}
	conn, err := dialFetch(addr, getenv)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	cl := applyfetchpb.NewFixtureFetchClient(conn)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	listed, err := cl.FetchFixtureFiles(ctx, &applyfetchpb.FetchFixtureFilesRequest{
		ProjectId: lk.ProjectID,
		Domain:    f.domain, // "" = every domain
	})
	if err != nil {
		return nil, fmt.Errorf("fixtures: list from the console: %w", err)
	}
	var seeds []migrate.FixtureSeed
	for _, file := range listed.GetFiles() {
		s := migrate.FixtureSeed{Domain: file.GetDomain(), Name: file.GetName()}
		if s.Group() != f.group {
			continue
		}
		seeds = append(seeds, s)
	}
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].Domain != seeds[j].Domain {
			return seeds[i].Domain < seeds[j].Domain
		}
		return seeds[i].Name < seeds[j].Name
	})
	for i := range seeds {
		resp, rerr := cl.FetchFixtureSeed(ctx, &applyfetchpb.FetchFixtureSeedRequest{
			ProjectId: lk.ProjectID,
			Domain:    seeds[i].Domain,
			Name:      seeds[i].Name,
		})
		if rerr != nil {
			return nil, fmt.Errorf("fixtures: render %s/%s: %w", seeds[i].Domain, seeds[i].Name, rerr)
		}
		seeds[i].Statements = resp.GetStatements()
	}
	fmt.Fprintf(out, "fixtures: rendered %d fixture(s) from the console (in memory; nothing written)\n", len(seeds))
	return seeds, nil
}

// openSeedTarget picks the connection to seed and opens it.
//
// Named explicitly with --connection, or inferred when the bundle owns exactly
// one store that could take rows. Inference stops at ONE on purpose: a bundle
// serving two databases has no defensible default, and picking the first would
// put a project's rows in whichever connection happened to sort earlier.
func openSeedTarget(specs []factory.TargetSpec, withoutDSN []string, want string) (string, migrate.SeedCapable, func(), error) {
	var candidates []factory.TargetSpec
	for _, s := range specs {
		if want != "" {
			if s.Connection == want {
				candidates = append(candidates, s)
			}
			continue
		}
		if factory.SeedCandidate(s.DSN) {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		// The diagnostic has to name what was SKIPPED for want of a DSN, not
		// only what was there. A connection dropped silently reads as a
		// connection the bundle does not serve, and sends the reader looking
		// at the lock instead of at the variable they did not set.
		unset := ""
		if len(withoutDSN) > 0 {
			unset = "\n  no DSN set for: " + strings.Join(withoutDSN, ", ")
		}
		if want != "" {
			return "", nil, nil, fmt.Errorf(
				"fixtures: --connection %q is not one this bundle serves with a DSN (have: %s)%s",
				want, connectionNames(specs), unset)
		}
		return "", nil, nil, fmt.Errorf(
			"fixtures: none of this bundle's connections (%s) is a store a fixture can be seeded into%s\n"+
				"  why: a fixture is relational rows; a KV or object store has nothing to insert them as",
			connectionNames(specs), unset)
	}
	if len(candidates) > 1 {
		return "", nil, nil, fmt.Errorf(
			"fixtures: this bundle serves %d seedable connections (%s) — say which one\n"+
				"  fix: --connection <name>",
			len(candidates), connectionNames(candidates))
	}
	spec := candidates[0]
	// The DSN filter above only narrowed the candidates. Whether the store can
	// actually take a seed is the applier's to answer, and it is the answer
	// acted on.
	ap, err := newApplierFor(specs)(spec.Connection)
	if err != nil {
		return "", nil, nil, fmt.Errorf("fixtures: open %s: %w", spec.Connection, err)
	}
	sc, ok := ap.(migrate.SeedCapable)
	if !ok {
		_ = ap.Close()
		return "", nil, nil, fmt.Errorf(
			"fixtures: connection %q takes no rendered fixture — its store has no seed support",
			spec.Connection)
	}
	return spec.Connection, sc, func() { _ = ap.Close() }, nil
}

// connectionNames renders a spec list for a diagnostic.
func connectionNames(specs []factory.TargetSpec) string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Connection)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// groupLabel renders a fixture group for human output; the default group has
// no name to print.
func groupLabel(group string) string {
	if group == "" {
		return "(default)"
	}
	return group
}
