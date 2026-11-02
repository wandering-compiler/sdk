package migrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// project lays out the default tree — fixtures/ beside w17/fixtures/ — and
// returns the rendered root plus a writer for authoring fixtures.
func project(t *testing.T) (rendered string, write func(rel, body string)) {
	t.Helper()
	root := t.TempDir()
	rendered = filepath.Join(root, "w17", "fixtures")
	if err := os.MkdirAll(rendered, 0o755); err != nil {
		t.Fatal(err)
	}
	write = func(rel, body string) {
		p := filepath.Join(root, "fixtures", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return rendered, write
}

func manifestFor(t *testing.T, rendered string, seeds map[string]migrate.RenderedSource) {
	t.Helper()
	if err := migrate.WriteRenderManifest(rendered, &migrate.RenderManifest{
		SourceRoot: "../../fixtures", Seeds: seeds,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStaleRenders_FreshRenderIsClean(t *testing.T) {
	rendered, write := project(t)
	write("app/acl-roles.json", `{"rows":[1]}`)
	write("app/demo/users.json", `{"rows":[2]}`)
	manifestFor(t, rendered, map[string]migrate.RenderedSource{
		"app/acl-roles":  {Source: "app/acl-roles.json", SHA256: migrate.FixtureDigest([]byte(`{"rows":[1]}`))},
		"app/demo/users": {Source: "app/demo/users.json", SHA256: migrate.FixtureDigest([]byte(`{"rows":[2]}`))},
	})
	for _, name := range []string{"acl-roles", "demo/users"} {
		if err := migrate.WriteFixtureSeed(rendered, migrate.FixtureSeed{Domain: "app", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := migrate.StaleRenders(rendered)
	if err != nil || len(got) != 0 {
		t.Fatalf("fresh render reported stale: %+v, %v", got, err)
	}
}

// The consumer's case: codegen rewrote acl-roles.json with a new permission
// and the render was not redone. Plus the two siblings of that drift.
func TestStaleRenders_ReportsChangedGoneAndUnrendered(t *testing.T) {
	rendered, write := project(t)
	write("app/acl-roles.json", `{"rows":["with permission 282"]}`)
	write("app/new.json", `{"rows":[]}`)
	manifestFor(t, rendered, map[string]migrate.RenderedSource{
		"app/acl-roles": {Source: "app/acl-roles.json", SHA256: migrate.FixtureDigest([]byte(`{"rows":["before"]}`))},
		"app/deleted":   {Source: "app/deleted.json", SHA256: "00"},
	})
	got, err := migrate.StaleRenders(rendered)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"app/acl-roles.json": migrate.StaleChanged,
		"app/deleted.json":   migrate.StaleSourceGone,
		"app/new.json":       migrate.StaleUnrendered,
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %v", got, want)
	}
	for _, s := range got {
		if want[s.Source] != s.Reason {
			t.Errorf("%s: reason %q, want %q", s.Source, s.Reason, want[s.Source])
		}
	}
	msg := migrate.FormatStaleRenders(got)
	if !strings.Contains(msg, "fixtures/app/acl-roles.json — "+migrate.StaleChanged) ||
		!strings.Contains(msg, "w17ctl fixtures render") {
		t.Errorf("message does not name the fixture and the fix:\n%s", msg)
	}
}

// "Cannot tell" never blocks: no manifest (a render older than this check),
// or no authoring tree beside the render (a deployed image).
func TestStaleRenders_CannotTellIsNotStale(t *testing.T) {
	rendered, write := project(t)
	write("app/acl-roles.json", `{}`)
	if got, err := migrate.StaleRenders(rendered); err != nil || got != nil {
		t.Fatalf("no manifest: %+v, %v", got, err)
	}

	image := t.TempDir() // rendered seeds only, no fixtures/ anywhere near
	manifestFor(t, image, map[string]migrate.RenderedSource{
		"app/acl-roles": {Source: "app/acl-roles.json", SHA256: "deadbeef"},
	})
	if got, err := migrate.StaleRenders(image); err != nil || got != nil {
		t.Fatalf("no authoring tree: %+v, %v", got, err)
	}
}

func TestRenderManifest_RoundTripsDeterministically(t *testing.T) {
	dir := t.TempDir()
	m := &migrate.RenderManifest{SourceRoot: "../../fixtures", Seeds: map[string]migrate.RenderedSource{
		"b/x": {Source: "b/x.json", SHA256: "2"}, "a/y": {Source: "a/y.json", SHA256: "1"},
	}}
	if err := migrate.WriteRenderManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, migrate.RenderManifestName))
	if err := migrate.WriteRenderManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, migrate.RenderManifestName))
	if string(first) != string(second) || strings.Index(string(first), `"a/y"`) > strings.Index(string(first), `"b/x"`) {
		t.Fatalf("not deterministic / not sorted:\n%s", first)
	}
	back, ok, err := migrate.ReadRenderManifest(dir)
	if err != nil || !ok || back.Seeds["a/y"].SHA256 != "1" || back.Version != migrate.RenderManifestVersion {
		t.Fatalf("round trip: %+v %v %v", back, ok, err)
	}
	// The seed loader must not mistake the manifest for a seed — an older
	// binary reading a new render depends on it.
	seeds, err := migrate.LoadFixtureSeeds(os.DirFS(dir), "", "")
	if err != nil || len(seeds) != 0 {
		t.Fatalf("manifest read as a seed: %+v %v", seeds, err)
	}
}

// A manifest entry is not proof the seed is there: a deleted seed whose entry
// and fixture remain must not read as fresh, or `apply` silently seeds nothing.
func TestStaleRenders_ADeletedSeedIsUnrendered(t *testing.T) {
	rendered, write := project(t)
	write("app/acl-roles.json", `{"rows":[1]}`)
	manifestFor(t, rendered, map[string]migrate.RenderedSource{
		"app/acl-roles": {Source: "app/acl-roles.json", SHA256: migrate.FixtureDigest([]byte(`{"rows":[1]}`))},
	})
	// No app/acl-roles.seed.json on disk.
	got, err := migrate.StaleRenders(rendered)
	if err != nil || len(got) != 1 || got[0].Reason != migrate.StaleUnrendered || got[0].Seed != "app/acl-roles" {
		t.Fatalf("deleted seed: %+v, %v", got, err)
	}
	// With the seed present, fresh.
	if err := migrate.WriteFixtureSeed(rendered, migrate.FixtureSeed{Domain: "app", Name: "acl-roles"}); err != nil {
		t.Fatal(err)
	}
	if got, err := migrate.StaleRenders(rendered); err != nil || len(got) != 0 {
		t.Fatalf("with the seed back: %+v, %v", got, err)
	}
}

// Only an ABSENT authoring tree means "cannot compare". One that exists but
// cannot be read as a directory is an error, not a silently disabled gate.
func TestStaleRenders_AnUnreadableSourceRootIsAnError(t *testing.T) {
	root := t.TempDir()
	rendered := filepath.Join(root, "w17", "fixtures")
	if err := os.MkdirAll(rendered, 0o755); err != nil {
		t.Fatal(err)
	}
	// A FILE where the fixtures directory should be.
	if err := os.WriteFile(filepath.Join(root, "fixtures"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestFor(t, rendered, map[string]migrate.RenderedSource{"app/x": {Source: "app/x.json", SHA256: "0"}})
	if _, err := migrate.StaleRenders(rendered); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a file as the source root was not an error: %v", err)
	}
}

// seedSchema is a recorded render schema: the renderer and model → digest.
func seedSchema(renderer string, tables ...string) *migrate.RenderedSchema {
	s := &migrate.RenderedSchema{Renderer: renderer, Tables: map[string]string{}}
	for i := 0; i+1 < len(tables); i += 2 {
		s.Tables[tables[i]] = tables[i+1]
	}
	return s
}

func currentFor(t *testing.T, rendered string, seeds map[string]migrate.CurrentSeedSchema) {
	t.Helper()
	if err := migrate.WriteCurrentSchema(rendered, &migrate.CurrentSchema{Seeds: seeds}); err != nil {
		t.Fatal(err)
	}
}

// schemaProject is a fresh render of one auth seed — the fixture unchanged
// since — recorded against `recorded`.
func schemaProject(t *testing.T, recorded *migrate.RenderedSchema) string {
	t.Helper()
	rendered, write := project(t)
	body := `{"rows":["user","membership"]}`
	write("basic/seed.json", body)
	manifestFor(t, rendered, map[string]migrate.RenderedSource{
		"basic/seed": {Source: "basic/seed.json", SHA256: migrate.FixtureDigest([]byte(body)), Schema: recorded},
	})
	if err := migrate.WriteFixtureSeed(rendered, migrate.FixtureSeed{Domain: "basic", Name: "seed"}); err != nil {
		t.Fatal(err)
	}
	return rendered
}

// The incident: the seed was rendered while auth.Membership had no foreign
// key, so the renderer ordered it before auth.User; a plugin release added the
// FK, the fixture did not change, and the seed read as fresh until apply.
func TestStaleRenders_AnFKAddedAfterTheRenderIsStale(t *testing.T) {
	rendered := schemaProject(t, seedSchema("1", "basic.Membership", "m-no-fk", "basic.User", "u"))
	currentFor(t, rendered, map[string]migrate.CurrentSeedSchema{
		"basic/seed": {Schema: seedSchema("1", "basic.Membership", "m-with-fk", "basic.User", "u")},
	})
	got, err := migrate.StaleRenders(rendered)
	if err != nil || len(got) != 1 {
		t.Fatalf("an FK added under an unchanged fixture: %+v, %v", got, err)
	}
	s := got[0]
	if s.Reason != migrate.StaleSchemaChanged || s.Seed != "basic/seed" || s.Source != "basic/seed.json" {
		t.Fatalf("wrong report: %+v", s)
	}
	if len(s.Changes) != 1 || !strings.Contains(s.Changes[0], "basic.Membership") {
		t.Fatalf("the change does not name the table: %q", s.Changes)
	}
	text := migrate.FormatStaleRenders(got)
	for _, want := range []string{"basic/seed.seed.json", migrate.StaleSchemaChanged, "basic.Membership", "w17ctl fixtures render"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
}

func TestStaleRenders_AnUnchangedSchemaIsFresh(t *testing.T) {
	rec := seedSchema("1", "basic.Membership", "m", "basic.User", "u")
	rendered := schemaProject(t, rec)
	currentFor(t, rendered, map[string]migrate.CurrentSeedSchema{
		"basic/seed": {Schema: seedSchema("1", "basic.Membership", "m", "basic.User", "u")},
	})
	if got, err := migrate.StaleRenders(rendered); err != nil || len(got) != 0 {
		t.Fatalf("unchanged schema reported stale: %+v, %v", got, err)
	}
}

// What else changes a render: the renderer, a table it no longer depends on,
// and one a new foreign key now reaches.
func TestStaleRenders_NamesEverySchemaChange(t *testing.T) {
	rendered := schemaProject(t, seedSchema("1", "basic.Membership", "m", "basic.Gone", "g"))
	currentFor(t, rendered, map[string]migrate.CurrentSeedSchema{
		"basic/seed": {Schema: seedSchema("2", "basic.Membership", "m", "basic.Org", "o")},
	})
	got, err := migrate.StaleRenders(rendered)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	joined := strings.Join(got[0].Changes, "\n")
	for _, want := range []string{"renderer changed (1 → 2)", "basic.Gone: no longer", "basic.Org: now a table"} {
		if !strings.Contains(joined, want) {
			t.Errorf("changes lack %q:\n%s", want, joined)
		}
	}
}

// A render from before schema provenance is stale ONCE against a known current
// schema — and not at all where nothing knows the current one.
func TestStaleRenders_AnOldManifestEntryIsStaleOnce(t *testing.T) {
	rendered := schemaProject(t, nil)
	if got, err := migrate.StaleRenders(rendered); err != nil || len(got) != 0 {
		t.Fatalf("no current schema is cannot-tell: %+v, %v", got, err)
	}
	currentFor(t, rendered, map[string]migrate.CurrentSeedSchema{"basic/seed": {}})
	if got, err := migrate.StaleRenders(rendered); err != nil || len(got) != 0 {
		t.Fatalf("a current schema without provenance (old console) is cannot-tell: %+v, %v", got, err)
	}
	currentFor(t, rendered, map[string]migrate.CurrentSeedSchema{"basic/seed": {Schema: seedSchema("1", "basic.User", "u")}})
	got, err := migrate.StaleRenders(rendered)
	if err != nil || len(got) != 1 || got[0].Reason != migrate.StaleSchemaUnrecorded {
		t.Fatalf("an old entry against a known schema: %+v, %v", got, err)
	}
}

// A fixture that no longer renders at all is stale, and the report says the
// fix starts with the fixture.
func TestStaleRenders_AFixtureThatNoLongerRendersIsStale(t *testing.T) {
	rendered := schemaProject(t, seedSchema("1", "basic.User", "u"))
	currentFor(t, rendered, map[string]migrate.CurrentSeedSchema{
		"basic/seed": {Error: `basic.User row 0: column "nick" not found`},
	})
	got, err := migrate.StaleRenders(rendered)
	if err != nil || len(got) != 1 || got[0].Reason != migrate.StaleSchemaBroken {
		t.Fatalf("got %+v, %v", got, err)
	}
	if text := migrate.FormatStaleRenders(got); !strings.Contains(text, `column "nick"`) || !strings.Contains(text, "edited") {
		t.Fatalf("report:\n%s", text)
	}
}

// The schema axis needs no authoring tree: a deployed image carries the
// render, its manifest and the current schema, and nothing else — which is
// where the incident's apply ran.
func TestStaleRenders_TheSchemaAxisWorksWithoutTheAuthoringTree(t *testing.T) {
	image := t.TempDir()
	manifestFor(t, image, map[string]migrate.RenderedSource{
		"basic/seed": {Source: "basic/seed.json", SHA256: "whatever", Schema: seedSchema("1", "basic.Membership", "m-no-fk")},
	})
	currentFor(t, image, map[string]migrate.CurrentSeedSchema{
		"basic/seed": {Schema: seedSchema("1", "basic.Membership", "m-with-fk")},
	})
	got, err := migrate.StaleRenders(image)
	if err != nil || len(got) != 1 || got[0].Reason != migrate.StaleSchemaChanged {
		t.Fatalf("image: %+v, %v", got, err)
	}
}

// A seed whose fixture changed is reported once, for the fixture: re-rendering
// fixes both, and two lines for one seed would read as two problems.
func TestStaleRenders_AChangedFixtureIsNotReportedTwice(t *testing.T) {
	rendered := schemaProject(t, seedSchema("1", "basic.User", "u"))
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(rendered, "..", "..", "fixtures", "basic", "seed.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"rows":["edited"]}`)
	currentFor(t, rendered, map[string]migrate.CurrentSeedSchema{"basic/seed": {Schema: seedSchema("1", "basic.User", "u2")}})
	got, err := migrate.StaleRenders(rendered)
	if err != nil || len(got) != 1 || got[0].Reason != migrate.StaleChanged {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestCurrentSchema_RoundTripsAndCompares(t *testing.T) {
	dir := t.TempDir()
	c := &migrate.CurrentSchema{Seeds: map[string]migrate.CurrentSeedSchema{
		"b/x": {Schema: seedSchema("1", "b.X", "1")}, "a/y": {Error: "boom"},
	}}
	if err := migrate.WriteCurrentSchema(dir, c); err != nil {
		t.Fatal(err)
	}
	back, ok, err := migrate.ReadCurrentSchema(dir)
	if err != nil || !ok || !migrate.SameCurrentSchema(c, back) {
		t.Fatalf("round trip: %+v %v %v", back, ok, err)
	}
	other := &migrate.CurrentSchema{Seeds: map[string]migrate.CurrentSeedSchema{
		"b/x": {Schema: seedSchema("1", "b.X", "2")}, "a/y": {Error: "boom"},
	}}
	if migrate.SameCurrentSchema(c, other) {
		t.Fatal("a changed digest compared equal")
	}
	// Invisible to the seed loader, like the manifest.
	if seeds, err := migrate.LoadFixtureSeeds(os.DirFS(dir), "", ""); err != nil || len(seeds) != 0 {
		t.Fatalf("current schema read as a seed: %+v %v", seeds, err)
	}
	if _, ok, err := migrate.ReadCurrentSchema(t.TempDir()); ok || err != nil {
		t.Fatalf("absent file: %v %v", ok, err)
	}
}

func TestRenderedSchemaOf(t *testing.T) {
	if migrate.RenderedSchemaOf(nil) != nil || migrate.RenderedSchemaOf(&applyfetchpb.SeedSchema{}) != nil {
		t.Fatal("an absent schema must stay absent (an older console)")
	}
	got := migrate.RenderedSchemaOf(&applyfetchpb.SeedSchema{Renderer: "1", Tables: []*applyfetchpb.SeedTableDigest{
		{Model: "basic.User", Table: "basic_user", Sha256: "u"},
	}})
	if got.Renderer != "1" || got.Tables["basic.User"] != "u" || len(got.Tables) != 1 {
		t.Fatalf("got %+v", got)
	}
}
