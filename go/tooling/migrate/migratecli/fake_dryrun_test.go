package migratecli

import (
	"context"
	"strings"
	"testing"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// --fake records a migration and --dry-run promises to write nothing: given
// both, the run is refused rather than silently picking one, so a rehearsal
// can never be told apart from the real thing only after the fact.
func TestApply_FakeWithDryRunIsRefused(t *testing.T) {
	lock := writeLock(t, migrate.LockConnection{Name: "core-postgres", TargetMigrationID: "ts-1", TargetContentSha256: "aa"})
	env := envMap{"W17_TARGET_CORE_POSTGRES": "postgres://u:p@127.0.0.1:1/db"}
	var out strings.Builder
	_, err := Dispatch(context.Background(),
		[]string{"migrate", "apply", "--lock", lock, "--migrations", t.TempDir(), "--fake", "--dry-run"},
		Options{Getenv: env.get, Out: &out})
	if err == nil || !strings.Contains(err.Error(), "--fake and --dry-run") {
		t.Fatalf("--fake with --dry-run was not refused for it: %v\n%s", err, out.String())
	}
}
