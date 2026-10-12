package grpcclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No internal-mesh dial path turns certificate verification off: the code
// that builds mesh TLS — this package and core/grpcx — never names
// InsecureSkipVerify (comments aside).
func TestMeshDialers_NeverSkipCertificateVerification(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "..", "core", "grpcx")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "InsecureSkipVerify" {
					t.Errorf("%s names InsecureSkipVerify", path)
				}
				return true
			})
		}
	}
}
