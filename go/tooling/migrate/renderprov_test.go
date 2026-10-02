package migrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if err != nil || !ok || back.Seeds["a/y"].SHA256 != "1" || back.Version != 1 {
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
