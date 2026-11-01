package migratecli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// A binary seeds its own store and nothing else.
//
// The image ships the project's WHOLE rendered fixtures tree, and `fixtures
// apply` with no --domain used to read every domain in it and hand all of it
// to this bundle's one store: `relation "auth_role" does not exist` where the
// other domain's table was absent, and a silent write where a same-named
// table was there. `w17ctl stack build` had the same defect and was fixed by
// routing on the connection the renderer names; the deployed binary was not.

// openedSeeder records which connection each seeding run opened.
type openedSeeder struct {
	rec    *recordingSeeder
	opened []string
}

func withOpenedSeeder(t *testing.T) *openedSeeder {
	t.Helper()
	o := &openedSeeder{rec: &recordingSeeder{}}
	prev := newApplierFor
	newApplierFor = func([]factory.TargetSpec) migrate.ApplierFor {
		return func(conn string) (migrate.Applier, error) {
			o.opened = append(o.opened, conn)
			return o.rec, nil
		}
	}
	t.Cleanup(func() { newApplierFor = prev })
	return o
}

func (o *openedSeeder) sql() []string {
	var out []string
	for _, call := range o.rec.calls {
		for _, s := range call {
			out = append(out, s.SQL)
		}
	}
	return out
}

func twoDomainTree(t *testing.T, withConnections bool) string {
	t.Helper()
	appConn, basicConn := "", ""
	if withConnections {
		appConn, basicConn = "app-postgres", "basic-postgres"
	}
	return seedDir(t,
		migrate.FixtureSeed{Domain: "app", Name: "seed", Connection: appConn,
			Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO app_item")}},
		migrate.FixtureSeed{Domain: "basic", Name: "seed", Connection: basicConn,
			Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO basic_tenant")}},
	)
}

var basicEnv = envMap{"W17_TARGET_BASIC_POSTGRES": "postgres://x/basic"}.get

// The default scope is the binary's own domain — with a render that names no
// store too (one made before the render recorded it), where the domain is
// the only thing that can tell the two apart.
func TestFixturesApply_DefaultsToTheBinarysOwnDomain(t *testing.T) {
	for _, named := range []bool{false, true} {
		o := withOpenedSeeder(t)
		dir := twoDomainTree(t, named)
		lock := writeLock(t, migrate.LockConnection{Name: "app-postgres"}, migrate.LockConnection{Name: "basic-postgres"})
		var out strings.Builder
		err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
			Options{Connections: []string{"basic-postgres"}, Domains: []string{"basic"}, Getenv: basicEnv, Out: &out}, &out)
		if err != nil {
			t.Fatalf("named=%v: runFixtures: %v\n%s", named, err, out.String())
		}
		if got := o.sql(); len(got) != 1 || got[0] != "INSERT INTO basic_tenant" {
			t.Errorf("named=%v: seeded %v, want only the basic domain's row", named, got)
		}
		if len(o.opened) != 1 || o.opened[0] != "basic-postgres" {
			t.Errorf("named=%v: opened %v, want basic-postgres", named, o.opened)
		}
	}
}

// --domain still narrows to one of the binary's own domains, and naming
// another one is refused before any store is opened.
func TestFixturesApply_AForeignDomainIsRefused(t *testing.T) {
	o := withOpenedSeeder(t)
	dir := twoDomainTree(t, false)
	lock := writeLock(t, migrate.LockConnection{Name: "basic-postgres"})
	opts := Options{Connections: []string{"basic-postgres"}, Domains: []string{"basic"}, Getenv: basicEnv}

	var out strings.Builder
	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir, "--domain", "app"}, opts, &out)
	if err == nil || !strings.Contains(err.Error(), `--domain "app" is not a domain this binary serves (it serves: basic)`) {
		t.Fatalf("want the foreign-domain refusal, got %v", err)
	}
	if len(o.opened) != 0 {
		t.Errorf("a refused run opened %v", o.opened)
	}

	if err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir, "--domain", "basic"}, opts, &out); err != nil {
		t.Fatalf("own --domain: %v", err)
	}
	if got := o.sql(); len(got) != 1 || got[0] != "INSERT INTO basic_tenant" {
		t.Errorf("own --domain seeded %v", got)
	}
}

// A binary that does not know its own domains (generated before Options
// carried them) still keeps another store's rows out, by the connection the
// render recorded: those are skipped and said so, and the rest go to the
// connection the render names — without a --connection, although the bundle
// serves two seedable stores.
func TestFixturesApply_UnknownIdentitySkipsOtherStoresByTheRenderedConnection(t *testing.T) {
	o := withOpenedSeeder(t)
	dir := seedDir(t,
		migrate.FixtureSeed{Domain: "app", Name: "seed", Connection: "app-postgres",
			Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO app_item")}},
		migrate.FixtureSeed{Domain: "basic", Name: "seed", Connection: "basic-postgres",
			Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO basic_tenant")}},
	)
	lock := writeLock(t, migrate.LockConnection{Name: "basic-postgres"}, migrate.LockConnection{Name: "basic-replica"})
	var out strings.Builder
	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Connections: []string{"basic-postgres", "basic-replica"},
			Getenv: envMap{"W17_TARGET_BASIC_POSTGRES": "postgres://x/b", "W17_TARGET_BASIC_REPLICA": "postgres://x/r"}.get}, &out)
	if err != nil {
		t.Fatalf("runFixtures: %v\n%s", err, out.String())
	}
	if got := o.sql(); len(got) != 1 || got[0] != "INSERT INTO basic_tenant" {
		t.Errorf("seeded %v, want only the basic row", got)
	}
	if len(o.opened) != 1 || o.opened[0] != "basic-postgres" {
		t.Errorf("opened %v, want the connection the render named", o.opened)
	}
	if !strings.Contains(out.String(), "skipped 1 fixture(s) of stores this binary does not serve: app/seed (app-postgres)") {
		t.Errorf("the skip was not reported:\n%s", out.String())
	}
}

// Where the scope was chosen, a seed whose render puts it on another store
// is a contradiction worth stopping on, not a row to drop quietly.
func TestFixturesApply_AnOwnDomainSeedOnAForeignStoreIsRefused(t *testing.T) {
	o := withOpenedSeeder(t)
	dir := seedDir(t, migrate.FixtureSeed{Domain: "basic", Name: "seed", Connection: "app-postgres",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO basic_tenant")}})
	lock := writeLock(t, migrate.LockConnection{Name: "basic-postgres"})
	var out strings.Builder
	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Connections: []string{"basic-postgres"}, Domains: []string{"basic"}, Getenv: basicEnv}, &out)
	if err == nil || !strings.Contains(err.Error(), `basic/seed belongs to connection "app-postgres", which this binary does not serve`) {
		t.Fatalf("want the foreign-store refusal, got %v", err)
	}
	if len(o.opened) != 0 {
		t.Errorf("a refused run opened %v", o.opened)
	}
}

// An explicit --connection the binary serves and can seed wins over the
// rendered name, with a note — it is the operator's correction for a render
// that named the wrong store.
func TestFixturesApply_ConnectionFlagOverridesTheRenderedStoreWithANote(t *testing.T) {
	o := withOpenedSeeder(t)
	dir := twoDomainTree(t, true)
	lock := writeLock(t, migrate.LockConnection{Name: "basic-postgres"}, migrate.LockConnection{Name: "basic-replica"})
	var out strings.Builder
	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir, "--connection", "basic-replica"},
		Options{Connections: []string{"basic-postgres", "basic-replica"}, Domains: []string{"basic"},
			Getenv: envMap{"W17_TARGET_BASIC_POSTGRES": "postgres://x/b", "W17_TARGET_BASIC_REPLICA": "postgres://x/r"}.get}, &out)
	if err != nil {
		t.Fatalf("runFixtures: %v\n%s", err, out.String())
	}
	if len(o.opened) != 1 || o.opened[0] != "basic-replica" {
		t.Errorf("opened %v, want the --connection the operator named", o.opened)
	}
	if !strings.Contains(out.String(), `--connection "basic-replica" overrides the store the render names ("basic-postgres")`) {
		t.Errorf("the override was not reported:\n%s", out.String())
	}
}

// A rendered name never makes a seed unseedable. An older console recorded a
// domain's FIRST relational store — app-mysql — for an acl-roles fixture whose
// tables are all on app-postgres; routing to it was refused as "no seed
// support" and role seeding stopped, with --connection app-postgres refused
// as a conflict. The unseedable name is set aside, the seedable connection is
// used, and --connection naming it is accepted.
func TestFixturesApply_AnUnseedableRenderedStoreDoesNotWin(t *testing.T) {
	dir := seedDir(t, migrate.FixtureSeed{Domain: "app", Name: "acl-roles", Connection: "app-mysql",
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO auth_role")}})
	lock := writeLock(t, migrate.LockConnection{Name: "app-mysql"}, migrate.LockConnection{Name: "app-postgres"})
	env := envMap{"W17_TARGET_APP_MYSQL": "mysql://u:p@tcp(db:3306)/app", "W17_TARGET_APP_POSTGRES": "postgres://x/app"}.get
	for _, args := range [][]string{nil, {"--connection", "app-postgres"}} {
		o := withOpenedSeeder(t)
		var out strings.Builder
		err := runFixtures(context.Background(), append([]string{"apply", "--lock", lock, "--fixtures", dir}, args...),
			Options{Connections: []string{"app-mysql", "app-postgres"}, Domains: []string{"app"}, Getenv: env}, &out)
		if err != nil {
			t.Fatalf("%v: runFixtures: %v\n%s", args, err, out.String())
		}
		if len(o.opened) != 1 || o.opened[0] != "app-postgres" {
			t.Errorf("%v: opened %v, want app-postgres", args, o.opened)
		}
		if got := o.sql(); len(got) != 1 {
			t.Errorf("%v: seeded %v", args, got)
		}
		if !strings.Contains(out.String(), `the render names "app-mysql" for these fixtures, a store with no seed support`) {
			t.Errorf("%v: the ignored name was not reported:\n%s", args, out.String())
		}
	}
}

// One run is one transaction on one store; seeds the render puts on two
// stores cannot be it.
func TestFixturesApply_SeedsOnTwoOwnedStoresAskForADomain(t *testing.T) {
	withOpenedSeeder(t)
	dir := twoDomainTree(t, true)
	lock := writeLock(t, migrate.LockConnection{Name: "app-postgres"}, migrate.LockConnection{Name: "basic-postgres"})
	var out strings.Builder
	err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir},
		Options{Connections: []string{"app-postgres", "basic-postgres"}, Domains: []string{"app", "basic"},
			Getenv: envMap{"W17_TARGET_APP_POSTGRES": "postgres://x/a", "W17_TARGET_BASIC_POSTGRES": "postgres://x/b"}.get}, &out)
	if err == nil || !strings.Contains(err.Error(), "belong to 2 stores (app-postgres, basic-postgres)") {
		t.Fatalf("want the two-stores refusal, got %v", err)
	}
}

// A dry run lists only what this binary would seed.
func TestFixturesApply_DryRunListsOnlyTheOwnDomain(t *testing.T) {
	dir := twoDomainTree(t, true)
	lock := writeLock(t, migrate.LockConnection{Name: "basic-postgres"})
	var out strings.Builder
	if err := runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fixtures", dir, "--dry-run"},
		Options{Connections: []string{"basic-postgres"}, Domains: []string{"basic"}, Getenv: basicEnv}, &out); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if strings.Contains(out.String(), "app/seed") || !strings.Contains(out.String(), "basic/seed") ||
		!strings.Contains(out.String(), "on basic-postgres") {
		t.Errorf("dry run listing:\n%s", out.String())
	}
}

// The rendered file carries the connection, and a render that has none
// writes the same bytes it always did.
func TestFixtureSeed_ConnectionRoundTrips(t *testing.T) {
	dir := seedDir(t,
		migrate.FixtureSeed{Domain: "app", Name: "named", Connection: "app-postgres", Statements: []*applyfetchpb.SeedStmt{seedStmt("X")}},
		migrate.FixtureSeed{Domain: "app", Name: "plain", Statements: []*applyfetchpb.SeedStmt{seedStmt("X")}},
	)
	got, err := migrate.LoadFixtureSeeds(os.DirFS(dir), "app", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Connection != "app-postgres" || got[1].Connection != "" {
		t.Fatalf("loaded %+v", got)
	}
	body, err := os.ReadFile(filepath.Join(dir, "app", "plain.seed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "connection") {
		t.Errorf("a seed with no connection grew a connection key:\n%s", body)
	}
}

// scopeIgnoringConsole answers the fetch surface as a console that ignores the
// domain filter — an older or misbehaving one — so the client's own scope is
// what is tested.
type scopeIgnoringConsole struct {
	applyfetchpb.UnimplementedFixtureFetchServer
	asked []string
}

func (c *scopeIgnoringConsole) FetchFixtureFiles(_ context.Context, r *applyfetchpb.FetchFixtureFilesRequest) (*applyfetchpb.FetchFixtureFilesResponse, error) {
	c.asked = append(c.asked, r.GetDomain())
	return &applyfetchpb.FetchFixtureFilesResponse{Files: []*applyfetchpb.FixtureFile{
		{Domain: "app", Name: "seed"}, {Domain: "basic", Name: "seed"},
	}}, nil
}

func (c *scopeIgnoringConsole) FetchFixtureSeed(_ context.Context, r *applyfetchpb.FetchFixtureSeedRequest) (*applyfetchpb.FetchFixtureSeedResponse, error) {
	return &applyfetchpb.FetchFixtureSeedResponse{
		Statements: []*applyfetchpb.SeedStmt{seedStmt("INSERT INTO " + r.GetDomain())},
		Connection: r.GetDomain() + "-postgres",
	}, nil
}

func TestFixturesApply_FetchSeedsOnlyTheOwnDomain(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	c := &scopeIgnoringConsole{}
	applyfetchpb.RegisterFixtureFetchServer(srv, c)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	o := withOpenedSeeder(t)
	lock := writeLock(t, migrate.LockConnection{Name: "basic-postgres"})
	var out strings.Builder
	err = runFixtures(context.Background(), []string{"apply", "--lock", lock, "--fetch", "--console", "grpc://" + lis.Addr().String()},
		Options{Connections: []string{"basic-postgres"}, Domains: []string{"basic"}, Getenv: basicEnv}, &out)
	if err != nil {
		t.Fatalf("fetch: %v\n%s", err, out.String())
	}
	if len(c.asked) != 1 || c.asked[0] != "basic" {
		t.Errorf("the console was asked for %q, want only the own domain", c.asked)
	}
	if got := o.sql(); len(got) != 1 || got[0] != "INSERT INTO basic" {
		t.Errorf("seeded %v, want only the basic row", got)
	}
	if len(o.opened) != 1 || o.opened[0] != "basic-postgres" {
		t.Errorf("opened %v", o.opened)
	}
}
