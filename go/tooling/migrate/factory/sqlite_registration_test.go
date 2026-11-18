package factory

import (
	"strings"
	"testing"
)

// Without tooling/migrate/sqlitedriver linked in, a SQLite target is refused
// with what to import, not taken for an unknown scheme. (The external tests of
// this package import the driver, so it is unregistered here by hand.)
func TestFromTargets_SQLiteWithoutTheDriverSaysWhatToImport(t *testing.T) {
	saved := sqliteDriver
	sqliteDriver.newApplier, sqliteDriver.newSnapshotter = nil, nil
	t.Cleanup(func() { sqliteDriver = saved })

	specs := []TargetSpec{{Connection: "main", DSN: "sqlite://" + t.TempDir() + "/x.db"}}
	if _, err := FromTargets(specs)("main"); err == nil || !strings.Contains(err.Error(), "tooling/migrate/sqlitedriver") {
		t.Errorf("applier: %v", err)
	}
	if _, err := SnapshotterFromTargets(specs)("main"); err == nil || !strings.Contains(err.Error(), "tooling/migrate/sqlitedriver") {
		t.Errorf("snapshotter: %v", err)
	}
}
