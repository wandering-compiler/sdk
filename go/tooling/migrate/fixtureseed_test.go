package migrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

func stmt(sql string, args ...any) *applyfetchpb.SeedStmt {
	vals := make([]*structpb.Value, 0, len(args))
	for _, a := range args {
		v, err := structpb.NewValue(a)
		if err != nil {
			panic(err)
		}
		vals = append(vals, v)
	}
	return &applyfetchpb.SeedStmt{Sql: sql, Args: vals}
}

// A rendered seed survives the round trip WITH its arguments beside the SQL.
//
// Storing flat SQL would be smaller and would be wrong: the values are data, a
// fixture author types some of them into a JSON file by hand, and the entire
// point of the rendering contract is that they never become SQL text. So the
// assertion is on the ARGUMENT, not on the statement count — a writer that
// interpolated and dropped the args would satisfy any check that only counted
// statements.
func TestFixtureSeed_RoundTripsArgumentsAndNotJustSQL(t *testing.T) {
	root := t.TempDir()
	const awkward = "O'Brien; DROP TABLE people;--"
	if err := migrate.WriteFixtureSeed(root, migrate.FixtureSeed{
		Domain:     "app",
		Name:       "people",
		Statements: []*applyfetchpb.SeedStmt{stmt(`INSERT INTO people (name) VALUES ($1)`, awkward)},
	}); err != nil {
		t.Fatalf("WriteFixtureSeed: %v", err)
	}
	got, err := migrate.LoadFixtureSeeds(os.DirFS(root), "", "")
	if err != nil {
		t.Fatalf("LoadFixtureSeeds: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d seed(s), want 1", len(got))
	}
	if got[0].Domain != "app" || got[0].Name != "people" {
		t.Errorf("identity lost: %+v", got[0])
	}
	args := got[0].Statements[0].GetArgs()
	if len(args) != 1 || args[0].GetStringValue() != awkward {
		t.Errorf("argument did not survive the round trip: %v", args)
	}
	if strings.Contains(got[0].Statements[0].GetSql(), "O'Brien") {
		t.Error("the value was folded into the SQL text")
	}
}

// The default group means the DEFAULT group, not "everything".
//
// This is the arm that decides which rows a plain seeding run puts in a
// database. A loader that returned every group would make a `demo` fixture —
// or a `prod` one — land wherever someone ran the command without a --group,
// and it would look like it worked.
func TestLoadFixtureSeeds_DefaultGroupDoesNotSweepUpNamedGroups(t *testing.T) {
	root := t.TempDir()
	for _, s := range []migrate.FixtureSeed{
		{Domain: "app", Name: "base", Statements: []*applyfetchpb.SeedStmt{stmt("SELECT 1")}},
		{Domain: "app", Name: "dev/extra", Statements: []*applyfetchpb.SeedStmt{stmt("SELECT 2")}},
		{Domain: "app", Name: "demo/showcase", Statements: []*applyfetchpb.SeedStmt{stmt("SELECT 3")}},
	} {
		if err := migrate.WriteFixtureSeed(root, s); err != nil {
			t.Fatalf("WriteFixtureSeed %s: %v", s.Name, err)
		}
	}
	def, err := migrate.LoadFixtureSeeds(os.DirFS(root), "", "")
	if err != nil {
		t.Fatalf("LoadFixtureSeeds: %v", err)
	}
	if len(def) != 1 || def[0].Name != "base" {
		t.Fatalf("default group = %v, want just [base]", names(def))
	}
	dev, err := migrate.LoadFixtureSeeds(os.DirFS(root), "", "dev")
	if err != nil {
		t.Fatalf("LoadFixtureSeeds(dev): %v", err)
	}
	if len(dev) != 1 || dev[0].Name != "dev/extra" {
		t.Fatalf("dev group = %v, want just [dev/extra]", names(dev))
	}
}

// Domain scoping narrows and does not merely annotate.
func TestLoadFixtureSeeds_DomainScopes(t *testing.T) {
	root := t.TempDir()
	for _, s := range []migrate.FixtureSeed{
		{Domain: "app", Name: "a", Statements: []*applyfetchpb.SeedStmt{stmt("SELECT 1")}},
		{Domain: "billing", Name: "b", Statements: []*applyfetchpb.SeedStmt{stmt("SELECT 2")}},
	} {
		if err := migrate.WriteFixtureSeed(root, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := migrate.LoadFixtureSeeds(os.DirFS(root), "billing", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Domain != "billing" {
		t.Fatalf("got %v, want just the billing one", names(got))
	}
}

// A fixture name is a registry key, not a path the caller steers. It arrives
// from the console, which is exactly the input a client must not trust
// structurally — a name of `../../etc/whatever` would otherwise write outside
// the artefact root.
func TestWriteFixtureSeed_RefusesPathShapedIdentifiers(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ domain, name string }{
		{"app", "../escape"},
		{"..", "ok"},
		{"app", "a/../../b"},
		{"app", "/absolute"},
	} {
		err := migrate.WriteFixtureSeed(root, migrate.FixtureSeed{
			Domain: tc.domain, Name: tc.name,
			Statements: []*applyfetchpb.SeedStmt{stmt("SELECT 1")},
		})
		if err == nil {
			t.Errorf("%s/%s was accepted", tc.domain, tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "identifier") {
			t.Errorf("%s/%s: diagnostic should say the identifier is the problem: %v", tc.domain, tc.name, err)
		}
	}
	// And nothing landed anywhere near the root's parent.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.seed.json")); err == nil {
		t.Error("a refused name still wrote a file outside the root")
	}
}

func names(seeds []migrate.FixtureSeed) []string {
	out := make([]string, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, s.Name)
	}
	return out
}

// The artefact's bytes are pinned, because "stable" cannot be observed from
// inside one process.
//
// protojson deliberately varies its whitespace to discourage treating its
// output as canonical — right for a wire format, wrong for a generated file
// that lands in a diff. Its variation is decided ONCE per process, though, so
// rendering the same seed a hundred times in one test agrees with itself and
// proves nothing; the churn only appears between CI runs, which is exactly
// where it appeared: every regeneration rewrote unchanged fixtures with
// shifted spaces, and a reviewer who learns to ignore those has learned to
// ignore the file.
//
// So the assertion is the exact bytes of a known seed. Any formatting that is
// not the deterministic encoder's fails it, in one process, today.
func TestWriteFixtureSeed_BytesArePinned(t *testing.T) {
	root := t.TempDir()
	if err := migrate.WriteFixtureSeed(root, migrate.FixtureSeed{
		Domain: "app", Name: "people",
		Statements: []*applyfetchpb.SeedStmt{stmt(`INSERT INTO people (id, name) VALUES ($1, $2)`, 7.0, "ada")},
	}); err != nil {
		t.Fatalf("WriteFixtureSeed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "app", "people.seed.json"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	const want = `{
  "statements": [
    {
      "args": [
        7,
        "ada"
      ],
      "sql": "INSERT INTO people (id, name) VALUES ($1, $2)"
    }
  ]
}
`
	if string(got) != want {
		t.Errorf("artefact bytes changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// An integer id is written as an integer, and the re-encoding does not turn it
// into a float or an exponent.
//
// Not cosmetic: the artefact is read by a person reviewing a fixture change,
// and `9.00001e+05` in place of `900001` is a row they cannot recognise.
//
// The ceiling here belongs to the CONTRACT, not to this writer: a SeedStmt
// argument is a google.protobuf.Value, i.e. a JSON value, so an integer above
// 2^53 has already lost precision before it reaches disk. Every id these
// fixtures pin is far below that, and a fixture that pinned a larger one would
// need the console's renderer to carry it as something other than a JSON
// number — a limit worth knowing about rather than one this file can fix.
func TestWriteFixtureSeed_IntegersStayIntegers(t *testing.T) {
	root := t.TempDir()
	if err := migrate.WriteFixtureSeed(root, migrate.FixtureSeed{
		Domain: "app", Name: "ids",
		Statements: []*applyfetchpb.SeedStmt{stmt(`INSERT INTO t (id) VALUES ($1)`, 900001.0)},
	}); err != nil {
		t.Fatalf("WriteFixtureSeed: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "app", "ids.seed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "900001") || strings.Contains(string(body), "e+") {
		t.Errorf("id is not written as a plain integer:\n%s", body)
	}
}
