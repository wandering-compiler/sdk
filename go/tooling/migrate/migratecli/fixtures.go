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
		// The FIXTURES usage: the migrate one lists verbs this command
		// does not have, and sends the reader to the wrong page.
		got := ""
		if len(args) > 0 {
			got = fmt.Sprintf(" (got %q)", args[0])
		}
		return fmt.Errorf("fixtures: the only subcommand is `apply`%s\n\n%s", got, fixturesUsage)
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
	if err := refuseSharedDSNVariable("fixtures", targets); err != nil {
		return err
	}
	specs, withoutDSN := seedSpecs(targets, opts.getenv())

	scope, err := seedScope(f.domain, opts.Domains)
	if err != nil {
		return err
	}
	seeds, err := loadSeeds(ctx, f, scope, lk, opts, out)
	if err != nil {
		return err
	}
	// Which store the rows go to, as the RENDERER says — the rule `w17ctl
	// stack build` follows. A fixture of another store never reaches this
	// one: refused, or skipped with a note when this binary cannot tell
	// which domains are its own.
	seeds, want, wantFromRender, err := routeSeeds(seeds, targets, scope, f.connection, out)
	if err != nil {
		return err
	}
	if len(seeds) == 0 {
		// Say WHERE it looked. "Nothing to seed" is a success, and a success
		// that names no source cannot be told apart from a run pointed at the
		// wrong directory or filtered by a group nobody rendered — which is
		// exactly how it read to a consumer, who took it as proof the step had
		// run. The three inputs that decide this answer are the
		// directory, the domain filter and the group, so all three are named.
		where := f.fixtures
		if f.fetch {
			where = "the console (--fetch)"
		}
		domain := scope.label()
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
		into := ""
		if want != "" {
			into = " on " + want
		}
		fmt.Fprintf(out, "fixtures: would apply %d fixture(s), %d statement(s) in group %s%s\n",
			len(seeds), len(stmts), groupLabel(f.group), into)
		for _, s := range seeds {
			fmt.Fprintf(out, "  %s/%s (%d statement(s))\n", s.Domain, s.Name, len(s.Statements))
		}
		return nil
	}

	conn, seeder, closeSeeder, err := openSeedTarget(specs, withoutDSN, want, wantFromRender)
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

// domainScope is the set of domains one seeding run reads fixtures for.
type domainScope struct {
	// domains, sorted; nil = every domain in the tree.
	domains []string
	// own: the scope is this binary's own domains (Options.Domains), not a
	// --domain the operator typed.
	own bool
}

// every reports whether the scope is unbounded — the binary does not know
// its own domains and no --domain was given.
func (s domainScope) every() bool { return len(s.domains) == 0 }

// has reports whether domain is in scope.
func (s domainScope) has(domain string) bool {
	if s.every() {
		return true
	}
	for _, d := range s.domains {
		if d == domain {
			return true
		}
	}
	return false
}

// label renders the scope for human output.
func (s domainScope) label() string {
	switch {
	case s.every():
		return "(every domain — this binary does not name its own)"
	case s.own:
		return strings.Join(s.domains, ", ") + " (this binary's own)"
	default:
		return strings.Join(s.domains, ", ")
	}
}

// seedScope resolves which domains' fixtures a run seeds.
//
// The image ships the project's whole rendered fixtures tree, so "every
// domain" used to be the default, and every domain's rows went into this
// bundle's one store. A binary that knows which domains it serves (own)
// defaults to exactly those, and refuses a --domain naming any other: that
// domain's rows belong in the store its own bundle owns, and seeding them
// here is either an error about a missing table or — where a same-named
// table exists — a silent write into the wrong database.
func seedScope(flagDomain string, own []string) (domainScope, error) {
	if len(own) == 0 {
		if flagDomain == "" {
			return domainScope{}, nil
		}
		return domainScope{domains: []string{flagDomain}}, nil
	}
	mine := append([]string(nil), own...)
	sort.Strings(mine)
	if flagDomain == "" {
		return domainScope{domains: mine, own: true}, nil
	}
	for _, d := range mine {
		if d == flagDomain {
			return domainScope{domains: []string{flagDomain}}, nil
		}
	}
	return domainScope{}, fmt.Errorf(
		"fixtures: --domain %q is not a domain this binary serves (it serves: %s)\n"+
			"  why: a binary seeds its own store; %q's rows belong in the store that domain's bundle owns,\n"+
			"       and seeding them here would write them into the wrong database\n"+
			"  fix: run `fixtures apply` with the %s bundle's binary, or drop --domain to seed %s",
		flagDomain, strings.Join(mine, ", "), flagDomain, flagDomain, strings.Join(mine, ", "))
}

// routeSeeds decides the store from what the renderer named, and keeps every
// other store's rows out of this one.
//
// The renderer records, beside each seed, the connection its domain lives on
// (FixtureSeed.Connection) — the same answer `w17ctl stack build` routes by.
// A seed naming a connection this binary does not serve is not this run's to
// apply: refused when the scope was chosen (the binary's own domains, or a
// --domain), and skipped with a note when the scope is "every domain" because
// the binary does not know its own — there the foreign seeds are expected, and
// refusing would fail every run of a multi-domain project.
//
// It returns the seeds to apply, the connection they go to ("" = not named
// by the render; openSeedTarget infers it as before) and whether that name
// came from the render rather than from --connection.
func routeSeeds(seeds []migrate.FixtureSeed, targets []migrate.ConnTarget, scope domainScope, flagConn string, out io.Writer) ([]migrate.FixtureSeed, string, bool, error) {
	owned := map[string]bool{}
	ownedNames := make([]string, 0, len(targets))
	for _, t := range targets {
		owned[t.Connection] = true
		ownedNames = append(ownedNames, t.Connection)
	}
	sort.Strings(ownedNames)
	var kept []migrate.FixtureSeed
	var skipped []string
	named := map[string]bool{}
	for _, s := range seeds {
		if s.Connection != "" && !owned[s.Connection] {
			if scope.every() {
				skipped = append(skipped, fmt.Sprintf("%s/%s (%s)", s.Domain, s.Name, s.Connection))
				continue
			}
			return nil, "", false, fmt.Errorf(
				"fixtures: %s/%s belongs to connection %q, which this binary does not serve (it serves: %s)\n"+
					"  why: the renderer put domain %q on that store; seeding it here would write its rows into the wrong database\n"+
					"  fix: seed it with the binary that serves %q",
				s.Domain, s.Name, s.Connection, strings.Join(ownedNames, ", "), s.Domain, s.Connection)
		}
		if s.Connection != "" {
			named[s.Connection] = true
		}
		kept = append(kept, s)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(out, "fixtures: skipped %d fixture(s) of stores this binary does not serve: %s\n"+
			"  note: each is seeded by the binary that serves its store\n",
			len(skipped), strings.Join(skipped, ", "))
	}
	switch len(named) {
	case 0:
		return kept, flagConn, false, nil
	case 1:
		var conn string
		for c := range named {
			conn = c
		}
		if flagConn != "" && flagConn != conn {
			return nil, "", false, fmt.Errorf(
				"fixtures: --connection %q, but the renderer put these fixtures on %q\n"+
					"  why: a fixture's rows belong in the store its domain lives on\n"+
					"  fix: drop --connection, or pass --connection %s",
				flagConn, conn, conn)
		}
		return kept, conn, flagConn == "", nil
	default:
		names := make([]string, 0, len(named))
		for c := range named {
			names = append(names, c)
		}
		sort.Strings(names)
		return nil, "", false, fmt.Errorf(
			"fixtures: the fixtures in scope belong to %d stores (%s), and one run seeds one store as one transaction\n"+
				"  fix: --domain <domain>, once per domain",
			len(names), strings.Join(names, ", "))
	}
}

// loadSeeds resolves the rendered fixtures, from disk or from the console.
func loadSeeds(ctx context.Context, f applyFlags, scope domainScope, lk *migrate.LockView, opts Options, out io.Writer) ([]migrate.FixtureSeed, error) {
	if !f.fetch {
		// A render older than its fixture seeds yesterday's rows — the roles
		// without the permission the new endpoint needs, and an endpoint that
		// answers PERMISSION_DENIED with nothing pointing at the seed (a
		// consumer, twice). Refuse when the authoring tree is beside the
		// render and disagrees with it; a deployed image carries only the
		// render, and there StaleRenders has nothing to compare and says so
		// by returning nothing.
		if !f.allowStale {
			if err := refuseStaleRender(f.fixtures, scope, f.group); err != nil {
				return nil, err
			}
		}
		fsys := os.DirFS(f.fixtures)
		// One read per domain in scope, so another domain's files are never
		// even parsed: a broken render in a domain this binary does not
		// serve is not this binary's failure.
		domains := scope.domains
		if scope.every() {
			domains = []string{""}
		}
		var seeds []migrate.FixtureSeed
		for _, d := range domains {
			got, err := migrate.LoadFixtureSeeds(fsys, d, f.group)
			if err != nil {
				return nil, fmt.Errorf(
					"fixtures: read %s: %w\n"+
						"  why: without --fetch the seeds are read from disk, where an earlier render wrote them\n"+
						"  fix: run the render step that populates %s, or pass --fetch to render over the wire",
					f.fixtures, err, f.fixtures)
			}
			seeds = append(seeds, got...)
		}
		return seeds, nil
	}
	return fetchSeeds(ctx, f, scope, lk, opts, out)
}

// fetchSeeds renders the group over the wire — the form for an environment
// that can reach the console and would rather not carry an artefact.
func fetchSeeds(ctx context.Context, f applyFlags, scope domainScope, lk *migrate.LockView, opts Options, out io.Writer) ([]migrate.FixtureSeed, error) {
	getenv := consoleAddrOf(f.console, opts.getenv())
	addr := getenv(envConsoleAddr)
	if addr == "" {
		return nil, fmt.Errorf(
			"fixtures: --fetch needs a console address — it is the console that renders a fixture into statements\n"+
				"  fix: set %s to the console's gRPC endpoint, or drop --fetch to read %s from disk",
			envConsoleAddr, f.fixtures)
	}
	conn, auth, err := dialFetch(addr, getenv)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	cl := applyfetchpb.NewFixtureFetchClient(conn)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	domains := scope.domains
	if scope.every() {
		domains = []string{""} // "" = every domain
	}
	var seeds []migrate.FixtureSeed
	for _, d := range domains {
		listed, err := cl.FetchFixtureFiles(ctx, &applyfetchpb.FetchFixtureFilesRequest{
			ProjectId: lk.ProjectID,
			Domain:    d,
		})
		if err != nil {
			return nil, fetchRefusal("fixtures", "list", addr, auth, err)
		}
		for _, file := range listed.GetFiles() {
			s := migrate.FixtureSeed{Domain: file.GetDomain(), Name: file.GetName()}
			// Checked here as well as asked of the console: the scope is
			// what keeps another domain's rows out of this store, and it
			// must not rest on the server honouring a filter.
			if s.Group() != f.group || !scope.has(s.Domain) {
				continue
			}
			seeds = append(seeds, s)
		}
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
			return nil, fetchRefusal("fixtures", "render of "+seeds[i].Domain+"/"+seeds[i].Name, addr, auth, rerr)
		}
		seeds[i].Statements = resp.GetStatements()
		seeds[i].Connection = resp.GetConnection()
	}
	fmt.Fprintf(out, "fixtures: rendered %d fixture(s) from the console (in memory; nothing written)\n", len(seeds))
	return seeds, nil
}

// openSeedTarget picks the connection to seed and opens it.
//
// Named by the render or with --connection, or inferred when the bundle owns
// exactly one store that could take rows. Inference stops at ONE on purpose: a bundle
// serving two databases has no defensible default, and picking the first would
// put a project's rows in whichever connection happened to sort earlier.
//
// want named by the RENDER (fromRender) is not a choice the operator made, so
// a refusal about it says where the name came from instead of blaming a
// --connection nobody typed.
func openSeedTarget(specs []factory.TargetSpec, withoutDSN []string, want string, fromRender bool) (string, migrate.SeedCapable, func(), error) {
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
		if want != "" && fromRender {
			return "", nil, nil, fmt.Errorf(
				"fixtures: the render puts these fixtures on connection %q, which this bundle serves with no DSN (have: %s)%s\n"+
					"  fix: set %s",
				want, connectionNames(specs), unset, migrate.TargetEnvVar(want))
		}
		if want != "" {
			return "", nil, nil, fmt.Errorf(
				"fixtures: --connection %q is not one this bundle serves with a DSN (have: %s)%s",
				want, connectionNames(specs), unset)
		}
		return "", nil, nil, fmt.Errorf(
			"fixtures: none of this bundle's connections (%s) is a store a fixture can be seeded into%s\n"+
				"  why: fixtures are seeded into Postgres only; MySQL, SQLite, KV and object stores have no seed support",
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
			"fixtures: connection %q takes no rendered fixture — its store has no seed support\n"+
				"  why: fixtures are seeded into Postgres only",
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

// refuseStaleRender fails when the rendered seeds this apply would read no
// longer match the fixtures they were rendered from. Scoped to what this run
// applies: a stale fixture in another domain, or in a group this run does not
// seed, is not this run's to refuse.
func refuseStaleRender(dir string, scope domainScope, group string) error {
	stale, err := migrate.StaleRenders(dir)
	if err != nil {
		return fmt.Errorf("fixtures: %w", err)
	}
	var mine []migrate.StaleRender
	for _, s := range stale {
		segs := strings.Split(s.Source, "/")
		if len(segs) < 2 {
			continue
		}
		if !scope.has(segs[0]) {
			continue
		}
		if strings.Join(segs[1:len(segs)-1], "/") != group {
			continue
		}
		mine = append(mine, s)
	}
	if len(mine) == 0 {
		return nil
	}
	return fmt.Errorf("fixtures: the rendered seeds in %s are older than the fixtures they came from:\n%s\n"+
		"  override: --allow-stale seeds from the render as it is",
		dir, migrate.FormatStaleRenders(mine))
}
