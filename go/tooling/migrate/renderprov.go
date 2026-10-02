package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A rendered seed is DERIVED from an authoring fixture, and nothing used to
// say from which bytes. So the two drifted silently, twice in one consumer's
// week, both times ending in a database that was seeded with yesterday's
// answer:
//
//   - codegen rewrote `fixtures/<domain>/acl-roles.json` with a permission
//     the new endpoint needed; the render under w17/fixtures was not redone;
//     the seeded roles lacked the permission and the endpoint answered
//     PERMISSION_DENIED. Nothing anywhere said the render was older than its
//     fixture.
//   - a fixture added and never rendered is simply not applied — `fixtures
//     apply` reads the rendered tree and has no idea a source exists.
//
// The render manifest is what makes "stale" a measurable fact: the render
// step records, per seed, the SHA-256 of the fixture file it rendered, and
// [StaleRenders] compares that against the fixture as it is now. It lives in
// its own file beside the seeds, NOT inside them: a seed is parsed with
// protojson, which refuses an unknown field, so a provenance key in the seed
// would make every binary built against an older SDK fail to read a new
// render. A file not ending in .seed.json is invisible to the seed loader.

// RenderManifestName is the manifest's file name inside the rendered root.
const RenderManifestName = ".rendered-from.json"

// RenderManifest records which fixture file, at which content, each rendered
// seed came from.
type RenderManifest struct {
	// Version of this format.
	Version int `json:"version"`
	// SourceRoot is the authoring fixtures root RELATIVE TO THE RENDERED ROOT,
	// slash-separated (`../../fixtures` for the default layout). Relative so
	// the check works from any working directory and in any checkout, as long
	// as the two trees keep their positions.
	SourceRoot string `json:"source_root"`
	// Seeds maps a seed's key (`<domain>/<name>`, the same encoding as its
	// path under the rendered root) to its source.
	Seeds map[string]RenderedSource `json:"seeds"`
}

// RenderedSource is one seed's provenance.
type RenderedSource struct {
	// Source is the fixture file relative to SourceRoot, slash-separated.
	Source string `json:"source"`
	// SHA256 is the hex digest of the fixture file's bytes at render time.
	SHA256 string `json:"sha256"`
}

// FixtureDigest is the digest a manifest records for a fixture's bytes.
func FixtureDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ReadRenderManifest reads the manifest under root. ok is false when there is
// none — a render made before manifests existed — which callers treat as
// "nothing to compare", never as stale.
func ReadRenderManifest(root string) (m *RenderManifest, ok bool, err error) {
	body, err := os.ReadFile(filepath.Join(root, RenderManifestName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", RenderManifestName, err)
	}
	m = &RenderManifest{}
	if err := json.Unmarshal(body, m); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", filepath.Join(root, RenderManifestName), err)
	}
	if m.Seeds == nil {
		m.Seeds = map[string]RenderedSource{}
	}
	return m, true, nil
}

// WriteRenderManifest writes m under root, deterministically (sorted keys,
// trailing newline): it is a generated file that lands in a diff.
func WriteRenderManifest(root string, m *RenderManifest) error {
	if m.Version == 0 {
		m.Version = 1
	}
	if m.Seeds == nil {
		m.Seeds = map[string]RenderedSource{}
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, RenderManifestName), append(buf, '\n'), 0o644)
}

// StaleRender is one seed, or one fixture, whose rendered tree no longer
// matches the authoring tree.
type StaleRender struct {
	// Seed is the rendered seed's key, empty for a fixture never rendered.
	Seed string
	// Source is the fixture file, relative to the source root.
	Source string
	// Path is the fixture file as the caller can open it (joined onto the
	// rendered root it passed in).
	Path string
	// Reason says which of the three it is.
	Reason string
}

// Reasons a render is stale.
const (
	StaleChanged    = "the fixture changed after it was rendered"
	StaleSourceGone = "the fixture it was rendered from no longer exists"
	StaleUnrendered = "the fixture has never been rendered"
)

// StaleRenders compares the rendered tree under root against the authoring
// tree its manifest names. It returns nothing — not an error — when there is
// no manifest (a render older than this check) or when the authoring tree is
// not there to compare against (a deployed image carries the rendered seeds
// only); both are "cannot tell", and "cannot tell" must not block a seed.
func StaleRenders(root string) ([]StaleRender, error) {
	m, ok, err := ReadRenderManifest(root)
	if err != nil || !ok {
		return nil, err
	}
	if m.SourceRoot == "" {
		return nil, nil
	}
	srcRoot := filepath.Join(root, filepath.FromSlash(m.SourceRoot))
	// Only an authoring tree that is genuinely ABSENT means "cannot compare"
	// (a deployed image). One that exists and cannot be read — permissions,
	// I/O, a file where the directory should be — is an error: swallowing it
	// would switch the gate off exactly when something is wrong.
	fi, err := os.Stat(srcRoot)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("fixtures source root %s: %w", srcRoot, err)
	case !fi.IsDir():
		return nil, fmt.Errorf("fixtures source root %s is not a directory (the manifest's source_root %q)", srcRoot, m.SourceRoot)
	}
	var out []StaleRender
	recorded := map[string]bool{}
	for key, rs := range m.Seeds {
		recorded[rs.Source] = true
		src := filepath.Join(srcRoot, filepath.FromSlash(rs.Source))
		body, err := os.ReadFile(src)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, StaleRender{Seed: key, Source: rs.Source, Path: src, Reason: StaleSourceGone})
			continue
		case err != nil:
			return nil, fmt.Errorf("read fixture %s: %w", rs.Source, err)
		case FixtureDigest(body) != rs.SHA256:
			out = append(out, StaleRender{Seed: key, Source: rs.Source, Path: src, Reason: StaleChanged})
			continue
		}
		// The manifest is not proof the seed exists. A deleted seed with its
		// entry and its fixture still in place would otherwise read as fresh
		// while `fixtures apply` silently loaded nothing for it.
		seedPath := filepath.Join(root, filepath.FromSlash(key)+fixtureSeedExt)
		sfi, err := os.Stat(seedPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, StaleRender{Seed: key, Source: rs.Source, Path: src, Reason: StaleUnrendered})
		case err != nil:
			return nil, fmt.Errorf("rendered seed %s: %w", seedPath, err)
		case !sfi.Mode().IsRegular():
			out = append(out, StaleRender{Seed: key, Source: rs.Source, Path: src, Reason: StaleUnrendered})
		}
	}
	// A fixture with no render at all. The authoring layout is
	// <root>/<domain>/[<group>/]<name>.json — a JSON file directly under the
	// root is not a fixture, which is also what the render step reads.
	walkErr := filepath.WalkDir(srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.Contains(rel, "/") || recorded[rel] {
			return nil
		}
		out = append(out, StaleRender{Source: rel, Path: p, Reason: StaleUnrendered})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

// FormatStaleRenders renders a stale list for a human: one line per fixture
// and the command that fixes it.
func FormatStaleRenders(stale []StaleRender) string {
	var b strings.Builder
	for _, s := range stale {
		fmt.Fprintf(&b, "  %s — %s\n", filepath.ToSlash(filepath.Clean(s.Path)), s.Reason)
	}
	b.WriteString("  fix: run `w17ctl fixtures render` and commit the result — the rendered seed is what a binary applies, so a stale one seeds yesterday's rows")
	return b.String()
}
