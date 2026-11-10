package randstr_test

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// Token's characters come from crypto/rand. No statistical test can tell a
// math/rand implementation apart, so the source is checked: the package
// imports crypto/rand and no math/rand.
func TestToken_SourceUsesCryptoRandOnly(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "randstr.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	crypto := false
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		switch path {
		case "crypto/rand":
			crypto = true
		case "math/rand", "math/rand/v2":
			t.Errorf("randstr.go imports %s; Token must draw from crypto/rand only", path)
		}
	}
	if !crypto {
		t.Error("randstr.go does not import crypto/rand")
	}
}
