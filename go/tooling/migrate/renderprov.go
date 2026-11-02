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

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
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
//
// The fixture's bytes are one of THREE inputs to a render, and they were long
// the only one recorded. The other two are the schema of the tables the render
// touches and the renderer itself: a seed rendered while the auth tables had no
// foreign keys was ordered alphabetically, a plugin release added the FKs, the
// fixture did not change — and the seed read as fresh until apply inserted a
// membership before its user. So a manifest entry also records
// [RenderedSchema]: the renderer's version and a digest per table the render
// depends on, as the console computed them (the client cannot — the IR is
// opaque to it). What they are compared AGAINST is [CurrentSchema], the same
// answer for the schema as it is now: written by `w17ctl codegen` and by the
// render itself into [CurrentSchemaName] beside the manifest, so the binary in
// a deployed image — no console, no authoring tree — can still tell.
//
// A plugin's version and activation reach a render through exactly one
// channel, the tables it puts into the activating domain's schema: a release
// that changes them changes their digests, an activation that adds or drops
// one changes the set. Recording the pin itself would add nothing a render
// depends on, and would mark every seed stale on every plugin release.

// RenderManifestName is the manifest's file name inside the rendered root.
const RenderManifestName = ".rendered-from.json"

// RenderManifestVersion is the manifest format this SDK writes. Version 2
// records each seed's schema; a version-1 entry has none, and is stale once
// against a known current schema (see [StaleSchemaUnrecorded]).
const RenderManifestVersion = 2

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
	// Schema is what the seed was rendered against. Nil from a render made
	// before it was recorded, or by a console that does not send it.
	Schema *RenderedSchema `json:"schema,omitempty"`
}

// RenderedSchema is the schema provenance of one render: the renderer's
// version and, per table the render depends on (keyed by fixture model,
// `<module>.<Message>`), the console's digest of that table's schema.
type RenderedSchema struct {
	Renderer string            `json:"renderer"`
	Tables   map[string]string `json:"tables"`
}

// RenderedSchemaOf converts the console's answer into the recorded form. Nil
// when the console sent none (it predates the field).
func RenderedSchemaOf(s *applyfetchpb.SeedSchema) *RenderedSchema {
	if s == nil || s.GetRenderer() == "" {
		return nil
	}
	out := &RenderedSchema{Renderer: s.GetRenderer(), Tables: map[string]string{}}
	for _, t := range s.GetTables() {
		out.Tables[t.GetModel()] = t.GetSha256()
	}
	return out
}

// CurrentSchemaName is the file, beside the manifest, holding what each seed
// WOULD be rendered against now.
const CurrentSchemaName = ".current-schema.json"

// CurrentSchema is, per rendered seed, the schema provenance a render of its
// fixture would record against the current schema. Written by `w17ctl
// codegen` (and by the render step), read by every staleness check — the
// binary's included, which has no console to ask.
type CurrentSchema struct {
	Version int                          `json:"version"`
	Seeds   map[string]CurrentSeedSchema `json:"seeds"`
}

// CurrentSeedSchema is one seed's entry in [CurrentSchema].
type CurrentSeedSchema struct {
	// Schema is what a render now would record. Nil when the console that
	// computed it does not send schema provenance — "cannot tell".
	Schema *RenderedSchema `json:"schema,omitempty"`
	// Error is set when the fixture no longer renders against the current
	// schema at all (a column it names was dropped, a model renamed). The
	// seed is stale, and re-rendering is not the whole fix.
	Error string `json:"error,omitempty"`
}

// ReadCurrentSchema reads [CurrentSchemaName] under root; ok is false when
// there is none.
func ReadCurrentSchema(root string) (c *CurrentSchema, ok bool, err error) {
	body, err := os.ReadFile(filepath.Join(root, CurrentSchemaName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", CurrentSchemaName, err)
	}
	c = &CurrentSchema{}
	if err := json.Unmarshal(body, c); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", filepath.Join(root, CurrentSchemaName), err)
	}
	if c.Seeds == nil {
		c.Seeds = map[string]CurrentSeedSchema{}
	}
	return c, true, nil
}

// WriteCurrentSchema writes c under root, deterministically (sorted keys,
// trailing newline): it is a generated file that lands in a diff.
func WriteCurrentSchema(root string, c *CurrentSchema) error {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Seeds == nil {
		c.Seeds = map[string]CurrentSeedSchema{}
	}
	buf, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, CurrentSchemaName), append(buf, '\n'), 0o644)
}

// SameCurrentSchema reports whether two current-schema records say the same
// thing, seed for seed.
func SameCurrentSchema(a, b *CurrentSchema) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Seeds) != len(b.Seeds) {
		return false
	}
	for k, av := range a.Seeds {
		bv, ok := b.Seeds[k]
		if !ok || av.Error != bv.Error || !sameRenderedSchema(av.Schema, bv.Schema) {
			return false
		}
	}
	return true
}

func sameRenderedSchema(a, b *RenderedSchema) bool {
	if a == nil || b == nil {
		return a == b
	}
	return len(SchemaChanges(a, b)) == 0
}

// SchemaChanges names, for a reader, what differs between the schema a seed
// was rendered against and the current one. Empty when they are the same.
func SchemaChanges(rendered, current *RenderedSchema) []string {
	var out []string
	if rendered.Renderer != current.Renderer {
		out = append(out, fmt.Sprintf("the fixture renderer changed (%s → %s)", rendered.Renderer, current.Renderer))
	}
	for _, model := range sortedKeysOf(rendered.Tables) {
		cur, ok := current.Tables[model]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s: no longer a table the render depends on (dropped, renamed, or no foreign key reaches it any more)", model))
		case cur != rendered.Tables[model]:
			out = append(out, fmt.Sprintf("%s: its table's schema changed (columns, types, defaults, keys or foreign keys)", model))
		}
	}
	for _, model := range sortedKeysOf(current.Tables) {
		if _, ok := rendered.Tables[model]; !ok {
			out = append(out, fmt.Sprintf("%s: now a table the render depends on (a foreign key reaches it)", model))
		}
	}
	return out
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
	m.Version = RenderManifestVersion
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
	// Reason says which kind of staleness it is.
	Reason string
	// Changes details a schema staleness: what differs, one line each.
	Changes []string
}

// Reasons a render is stale.
const (
	StaleChanged    = "the fixture changed after it was rendered"
	StaleSourceGone = "the fixture it was rendered from no longer exists"
	StaleUnrendered = "the fixture has never been rendered"
	// StaleSchemaChanged: the fixture is the same, the schema it was rendered
	// against is not — the statements, and their order above all, may be wrong.
	StaleSchemaChanged = "the schema it was rendered against changed"
	// StaleSchemaUnrecorded: the render predates schema provenance, so nothing
	// says what it was rendered against. Rendering once more records it.
	StaleSchemaUnrecorded = "it was rendered before a render recorded its schema — render it once more to record it"
	// StaleSchemaBroken: the fixture no longer renders against the current
	// schema at all.
	StaleSchemaBroken = "the fixture no longer renders against the current schema"
)

// StaleRenders compares the rendered tree under root against the authoring
// tree its manifest names. It returns nothing — not an error — when there is
// no manifest (a render older than this check) or when the authoring tree is
// not there to compare against (a deployed image carries the rendered seeds
// only); both are "cannot tell", and "cannot tell" must not block a seed.
//
// The schema axis is separate and needs no authoring tree: it compares each
// entry's recorded [RenderedSchema] with [CurrentSchemaName] beside it, so a
// deployed image is checked too. No current-schema file, or no entry in it for
// a seed, is "cannot tell" on that axis.
func StaleRenders(root string) ([]StaleRender, error) {
	cur, ok, err := ReadCurrentSchema(root)
	if err != nil {
		return nil, err
	}
	if !ok {
		cur = nil
	}
	return StaleRendersAgainst(root, cur)
}

// StaleRendersAgainst is [StaleRenders] with the current schema supplied by
// the caller (verify computes it live) instead of read from disk. A nil cur
// skips the schema axis.
func StaleRendersAgainst(root string, cur *CurrentSchema) ([]StaleRender, error) {
	m, ok, err := ReadRenderManifest(root)
	if err != nil || !ok {
		return nil, err
	}
	out, err := staleFixtures(root, m)
	if err != nil {
		return nil, err
	}
	out = append(out, staleSchemas(root, m, cur, out)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

// staleSchemas is the schema axis: every seed the fixture axis did not already
// report whose recorded schema differs from the current one.
func staleSchemas(root string, m *RenderManifest, cur *CurrentSchema, already []StaleRender) []StaleRender {
	if cur == nil {
		return nil
	}
	reported := map[string]bool{}
	for _, s := range already {
		if s.Seed != "" {
			reported[s.Seed] = true
		}
	}
	var out []StaleRender
	for _, key := range sortedKeysOf(m.Seeds) {
		if reported[key] {
			continue
		}
		rs := m.Seeds[key]
		now, ok := cur.Seeds[key]
		if !ok {
			continue
		}
		st := StaleRender{Seed: key, Source: rs.Source, Path: filepath.Join(root, filepath.FromSlash(key)+fixtureSeedExt)}
		switch {
		case now.Error != "":
			st.Reason, st.Changes = StaleSchemaBroken, []string{now.Error}
		case now.Schema == nil:
			continue // computed by a console that sends no schema: cannot tell
		case rs.Schema == nil:
			st.Reason = StaleSchemaUnrecorded
		default:
			changes := SchemaChanges(rs.Schema, now.Schema)
			if len(changes) == 0 {
				continue
			}
			st.Reason, st.Changes = StaleSchemaChanged, changes
		}
		out = append(out, st)
	}
	return out
}

// staleFixtures is the fixture axis: the render against the authoring tree.
func staleFixtures(root string, m *RenderManifest) ([]StaleRender, error) {
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
	return out, nil
}

// FormatStaleRenders renders a stale list for a human: one line per fixture
// and the command that fixes it.
func FormatStaleRenders(stale []StaleRender) string {
	var b strings.Builder
	broken := false
	for _, s := range stale {
		fmt.Fprintf(&b, "  %s — %s\n", filepath.ToSlash(filepath.Clean(s.Path)), s.Reason)
		for _, c := range s.Changes {
			fmt.Fprintf(&b, "      %s\n", c)
		}
		broken = broken || s.Reason == StaleSchemaBroken
	}
	if broken {
		b.WriteString("  a fixture that no longer renders has to be edited to match the schema first\n")
	}
	b.WriteString("  fix: run `w17ctl fixtures render --prune` and commit the result — the rendered seed is what a binary applies, so a stale one seeds yesterday's rows (--prune also drops the seeds whose fixture is gone)")
	return b.String()
}
