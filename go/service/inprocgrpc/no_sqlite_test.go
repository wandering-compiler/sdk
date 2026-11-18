package inprocgrpc_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The packages every generated server links do not depend on
// modernc.org/sqlite: SQLite in pure Go is the largest module the SDK
// requires, and a project on PostgreSQL or MySQL would download and compile
// it on every cold build. A project with a SQLite connection gets it through
// its generated storage code and tooling/migrate/sqlitedriver.
func TestServerPackagesDoNotPullInSQLite(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	for _, pkg := range []string{
		"github.com/wandering-compiler/sdk/go/core/grpcerr",
		"github.com/wandering-compiler/sdk/go/service/inprocgrpc",
		"github.com/wandering-compiler/sdk/go/service/bootstrap",
		"github.com/wandering-compiler/sdk/go/tooling/migrate/migratecli",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			if strings.HasPrefix(dep, "modernc.org/") {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
}
