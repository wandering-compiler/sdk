package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// TestApply_FKEnforced_NonRebuild (B4, Fable audit T1-3 pass #1) — a plain
// (non-rebuild) migration must run with foreign-key enforcement ON, so an
// INSERT that violates an FK fails atomically. The applier used to wrap EVERY
// migration in `PRAGMA foreign_keys=OFF`, silently disabling enforcement — a
// bad-FK row slipped in unnoticed.
func TestApply_FKEnforced_NonRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fk.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	ctx := context.Background()

	// Schema: child.pid REFERENCES parent(id).
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "m1",
		UpSql: "BEGIN;\n" +
			"CREATE TABLE parent (id INTEGER PRIMARY KEY);\n" +
			"CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id));\n" +
			"COMMIT;",
	}); err != nil {
		t.Fatalf("Apply schema: %v", err)
	}

	// Insert a child row pointing at a non-existent parent — must be rejected.
	err = a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "m2",
		UpSql: "BEGIN;\nINSERT INTO child (id, pid) VALUES (1, 999);\nCOMMIT;",
	})
	if err == nil {
		t.Fatal("B4: a migration inserting an FK-violating row must fail (foreign keys enforced), but Apply succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("B4: expected a foreign-key constraint error, got: %v", err)
	}
}

// TestApply_RebuildFKViolation_Caught (B4) — a 12-step rebuild runs its body
// under `foreign_keys=OFF`, so a rebuild that adds a stricter FK over data
// that already violates it is NOT rejected during the copy. The emit body ends
// with `PRAGMA foreign_key_check` before COMMIT, but that runs via Exec (rows
// discarded). The applier must re-run it as a QUERY and fail. Without the fix
// the migration succeeds and leaves a silently-inconsistent DB.
func TestApply_RebuildFKViolation_Caught(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rebuild.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	ctx := context.Background()

	// Seed: parent + child WITHOUT an FK yet, and a child row whose pid has no
	// parent (legal today — no constraint).
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "m1",
		UpSql: "BEGIN;\n" +
			"CREATE TABLE parent (id INTEGER PRIMARY KEY);\n" +
			"CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER);\n" +
			"INSERT INTO child (id, pid) VALUES (1, 999);\n" +
			"COMMIT;",
	}); err != nil {
		t.Fatalf("Apply seed: %v", err)
	}

	// Rebuild migration (SQLite 12-step shape, emit's foreign-key bracket):
	// recreate `child` WITH an FK pid→parent(id). The existing pid=999 row now
	// violates it; foreign_key_check inside the recipe finds it.
	rebuildUp := "PRAGMA foreign_keys=OFF;\n\n" +
		"BEGIN;\n\n" +
		"CREATE TABLE child_new (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id));\n" +
		"INSERT INTO child_new (id, pid) SELECT id, pid FROM child;\n" +
		"DROP TABLE child;\n" +
		"ALTER TABLE child_new RENAME TO child;\n\n" +
		"PRAGMA foreign_key_check;\n\n" +
		"COMMIT;\n\n" +
		"PRAGMA foreign_keys=ON;\n"
	err = a.Apply(ctx, &applyfetchpb.Migration{Id: "m2", UpSql: rebuildUp})
	if err == nil {
		t.Fatal("B4: a rebuild that adds an FK violated by existing data must fail (foreign_key_check), but Apply succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign-key violation") {
		t.Errorf("B4: expected a foreign_key_check violation error, got: %v", err)
	}
}

// TestApply_RebuildFKClean_Succeeds (B4 guard) — the same rebuild over CLEAN
// data (the child row points at a real parent) must succeed: foreign_key_check
// finds nothing, and running the rebuild under foreign_keys=OFF must not itself
// be an error.
func TestApply_RebuildFKClean_Succeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rebuild_ok.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	ctx := context.Background()

	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "m1",
		UpSql: "BEGIN;\n" +
			"CREATE TABLE parent (id INTEGER PRIMARY KEY);\n" +
			"CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER);\n" +
			"INSERT INTO parent (id) VALUES (999);\n" +
			"INSERT INTO child (id, pid) VALUES (1, 999);\n" +
			"COMMIT;",
	}); err != nil {
		t.Fatalf("Apply seed: %v", err)
	}

	rebuildUp := "PRAGMA foreign_keys=OFF;\n\n" +
		"BEGIN;\n\n" +
		"CREATE TABLE child_new (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id));\n" +
		"INSERT INTO child_new (id, pid) SELECT id, pid FROM child;\n" +
		"DROP TABLE child;\n" +
		"ALTER TABLE child_new RENAME TO child;\n\n" +
		"PRAGMA foreign_key_check;\n\n" +
		"COMMIT;\n\n" +
		"PRAGMA foreign_keys=ON;\n"
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "m2", UpSql: rebuildUp}); err != nil {
		t.Fatalf("B4 guard: clean rebuild must succeed, got: %v", err)
	}
}
