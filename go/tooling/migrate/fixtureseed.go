package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// A rendered fixture on disk, and why fixtures need one at all.
//
// Rendering a fixture is schema-aware — table and column resolution, FK load
// order, value binding — and that knowledge is the compiler's, which is why
// the console does it. The process that can APPLY the result is the binary
// that owns the database. Between those two facts sits an environment where
// neither is true at the same time: a compose stack, a CI job, an air-gapped
// deploy. There the console ran at BUILD time and is long gone by the time the
// database exists.
//
// Migrations solved this by writing the console's answer down (`migrate fetch`
// → a directory the apply step reads). This is the same artefact for fixtures,
// deliberately in the same shape, so "seed the database" has the same two
// forms as "migrate the database": read what was rendered earlier, or render
// it now over the wire.
//
// The seed is stored PARAMETERIZED — statements with $1..$N and their values
// beside them — rather than as flat SQL. Flattening would be smaller and would
// be wrong: the values are data, some of them come from a fixture author
// typing into a JSON file, and the point of the whole rendering contract is
// that they never become SQL text.

// fixtureSeedExt is the suffix that marks a rendered seed, so a fixtures
// directory can hold the authoring JSON beside it without the loader guessing.
const fixtureSeedExt = ".seed.json"

// FixtureSeed is one rendered fixture: its identity and the statements to run.
type FixtureSeed struct {
	// Domain the fixture belongs to.
	Domain string
	// Name is the FLAT registry key — `<group>/<leaf>` for a grouped fixture,
	// `<leaf>` for the default group. Same encoding the registry and
	// `fixtures push` use, so an artefact directory and the registry name the
	// same fixture the same way.
	Name string
	// Statements run in order: FK targets before referrers.
	Statements []*applyfetchpb.SeedStmt
}

// Group is the group Name belongs to — everything up to the last "/", empty
// for the default group.
func (f FixtureSeed) Group() string {
	if i := strings.LastIndex(f.Name, "/"); i >= 0 {
		return f.Name[:i]
	}
	return ""
}

// WriteFixtureSeed writes one rendered fixture under root as
// `<root>/<domain>/<name>.seed.json`, creating the directories it needs.
func WriteFixtureSeed(root string, seed FixtureSeed) error {
	if seed.Domain == "" {
		return fmt.Errorf("WriteFixtureSeed: empty domain")
	}
	if seed.Name == "" {
		return fmt.Errorf("WriteFixtureSeed: empty name")
	}
	// A name is a registry key, not a path the caller gets to steer. Without
	// this, a fixture named `../../etc/whatever` would write outside root —
	// and the name arrives from the console, which is exactly the kind of
	// input a client is not supposed to trust structurally.
	if err := checkSeedSegments(seed.Domain, seed.Name); err != nil {
		return fmt.Errorf("WriteFixtureSeed: %w", err)
	}
	rel := filepath.FromSlash(seed.Domain + "/" + seed.Name + fixtureSeedExt)
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
	}
	buf, err := marshalSeedStable(seed)
	if err != nil {
		return fmt.Errorf("marshal %s/%s: %w", seed.Domain, seed.Name, err)
	}
	// Trailing newline: this is a generated file that lands in a diff, and a
	// last line without one shows up as "\ No newline at end of file" in every
	// review of every fixture change.
	return os.WriteFile(full, append(buf, '\n'), 0o644)
}

// marshalSeedStable renders a seed as JSON that is the SAME bytes every time.
//
// protojson deliberately varies its whitespace between runs to discourage
// treating its output as canonical — which is exactly right for a wire format
// and exactly wrong for a generated file that lands in a diff. Left alone, it
// made every regeneration of an unchanged fixture produce a churn diff of
// shifted spaces, and a reviewer who learns to ignore those has learned to
// ignore the file.
//
// So the protojson output is re-encoded through encoding/json, which sorts
// object keys and indents deterministically. Numbers go through json.Number so
// a 64-bit id is not rounded through a float on the way.
func marshalSeedStable(seed FixtureSeed) ([]byte, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.
		Marshal(&applyfetchpb.FetchFixtureSeedResponse{Statements: seed.Statements})
	if err != nil {
		return nil, err
	}
	return stableJSON(raw)
}

// stableJSON re-encodes protojson output deterministically: encoding/json
// sorts object keys and indents the same way every run, and json.Number keeps
// a 64-bit value off the float path on the way through.
func stableJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return json.MarshalIndent(doc, "", "  ")
}

// LoadFixtureSeeds reads every rendered seed under fsys, optionally narrowed
// to one domain and one group. Results come back in (domain, name) order,
// which is the order they apply in.
//
// An empty `group` means the DEFAULT group and matches only ungrouped
// fixtures — not "every group". That is the same reading `w17ctl fixtures
// apply` has, and it matters: a caller asking for the default set must not
// silently pick up a `demo` group whose rows were never meant for it.
func LoadFixtureSeeds(fsys fs.FS, domain, group string) ([]FixtureSeed, error) {
	var out []FixtureSeed
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, fixtureSeedExt) {
			return nil
		}
		dom, name, ok := strings.Cut(strings.TrimSuffix(p, fixtureSeedExt), "/")
		if !ok || dom == "" || name == "" {
			return fmt.Errorf("fixture seed %s: expected <domain>/<name>%s", p, fixtureSeedExt)
		}
		if domain != "" && dom != domain {
			return nil
		}
		seed := FixtureSeed{Domain: dom, Name: name}
		if seed.Group() != group {
			return nil
		}
		body, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", p, rerr)
		}
		var resp applyfetchpb.FetchFixtureSeedResponse
		if uerr := protojson.Unmarshal(body, &resp); uerr != nil {
			return fmt.Errorf("parse %s: %w", p, uerr)
		}
		seed.Statements = resp.GetStatements()
		out = append(out, seed)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// checkSeedSegments refuses a domain or name whose path segments could escape
// the artefact root or collide with the filesystem's own vocabulary.
func checkSeedSegments(domain, name string) error {
	for _, seg := range append(strings.Split(domain, "/"), strings.Split(name, "/")...) {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, `\:`) {
			return fmt.Errorf("path-shaped identifier %q/%q: segment %q is not a name", domain, name, seg)
		}
	}
	if path.IsAbs(domain) || path.IsAbs(name) {
		return fmt.Errorf("absolute identifier %q/%q", domain, name)
	}
	return nil
}

// LoadFixtureSeedNames lists the rendered seeds under fsys by identity alone,
// without reading their bodies — what a render step needs to know which
// artefacts it did not just produce.
//
// A missing root is an EMPTY list rather than an error: "nothing rendered yet"
// and "nothing to prune" are the same answer, and a first run must not have to
// special-case its own directory into existence.
func LoadFixtureSeedNames(fsys fs.FS) ([]FixtureSeed, error) {
	var out []FixtureSeed
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, fixtureSeedExt) {
			return nil
		}
		dom, name, ok := strings.Cut(strings.TrimSuffix(p, fixtureSeedExt), "/")
		if !ok || dom == "" || name == "" {
			return fmt.Errorf("fixture seed %s: expected <domain>/<name>%s", p, fixtureSeedExt)
		}
		out = append(out, FixtureSeed{Domain: dom, Name: name})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
