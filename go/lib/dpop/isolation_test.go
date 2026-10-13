package dpop_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// DPoP is opt-in down to the module graph: only lib/dpop, its subpackages and
// lib/restgw/dpopgw may import lib/dpop or go-jose. Every other package —
// lib/restgw, lib/principal, core/grpcx and the rest — must not, so a
// consumer that never uses DPoP gets no new transitive dependency (and no
// new go.sum entry) when it bumps the SDK. Checked on the direct imports of
// every non-test file in the module; since go-jose is reachable only through
// these packages, that bounds the transitive graph too.
func TestDPoPImportsStayInsideTheOptInPackages(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{"lib/dpop", "lib/restgw/dpopgw"}
	forbidden := []string{"github.com/wandering-compiler/sdk/go/lib/dpop", "github.com/go-jose/go-jose"}
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, a := range allowed {
			if strings.HasPrefix(rel, a+"/") {
				return nil
			}
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if p == bad || strings.HasPrefix(p, bad+"/") {
					t.Errorf("%s imports %s: DPoP must stay in its opt-in packages", rel, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("checked only %d files; the walk did not reach the module", checked)
	}
}
