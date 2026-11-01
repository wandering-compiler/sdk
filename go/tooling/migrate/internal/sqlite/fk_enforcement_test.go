package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// TestApply_FKEnforced_NonRebuild — a plain
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

// fkRebuild is the SQLite 12-step shape (emit's foreign-key bracket) that
// re-creates `table` as create, copying the rows over when copy is set.
func fkRebuild(table, create string, copyRows bool) string {
	body := "PRAGMA foreign_keys=OFF;\n\nBEGIN;\n\n" + create + "\n"
	if copyRows {
		body += "INSERT INTO " + table + "_new SELECT * FROM " + table + ";\n"
	}
	return body + "DROP TABLE " + table + ";\n" +
		"ALTER TABLE " + table + "_new RENAME TO " + table + ";\n\n" +
		"PRAGMA foreign_key_check;\n\nCOMMIT;\n\nPRAGMA foreign_keys=ON;\n"
}

func fkApplier(t *testing.T) (*sqlite.Applier, context.Context) {
	t.Helper()
	a, err := sqlite.New(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "rebuild.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, context.Background()
}

// TestApply_RebuildAddsFKOverOrphans_Reports — a rebuild that ADDS a key over
// a table already holding a row that references nothing (an orphan) applies:
// the migrator adds such a key without refusing the rows it finds
// (fk-add-over-existing-rows; Postgres adds it NOT VALID, MySQL with the
// checks off). The orphan stays, and a new one is still refused.
func TestApply_RebuildAddsFKOverOrphans_Reports(t *testing.T) {
	a, ctx := fkApplier(t)
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
	up := fkRebuild("child", "CREATE TABLE child_new (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id));", true)
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "m2", UpSql: up}); err != nil {
		t.Fatalf("a rebuild that adds a key over existing orphans must apply, got: %v", err)
	}
	// A later rebuild of the same table keeps the earlier orphan, introduces
	// nothing, and applies.
	again := fkRebuild("child", "CREATE TABLE child_new (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id));", true)
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "m3", UpSql: again}); err != nil {
		t.Fatalf("a rebuild that only carries an earlier orphan must apply, got: %v", err)
	}
	err := a.Apply(ctx, &applyfetchpb.Migration{Id: "m4", UpSql: "BEGIN;\nINSERT INTO child (id, pid) VALUES (2, 998);\nCOMMIT;"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("the added key must refuse a NEW orphan, got: %v", err)
	}
}

// TestApply_RebuildFKViolation_Caught (B4) — a 12-step rebuild runs its body
// under `foreign_keys=OFF`, so a rebuild that breaks a key the database
// already enforced is NOT rejected during the copy. The emit body ends with
// `PRAGMA foreign_key_check` before COMMIT, but that runs via Exec (rows
// discarded). The applier must re-run it as a QUERY and fail. Here the parent
// is re-created without its rows, orphaning a child the key held.
func TestApply_RebuildFKViolation_Caught(t *testing.T) {
	a, ctx := fkApplier(t)
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "m1",
		UpSql: "BEGIN;\n" +
			"CREATE TABLE parent (id INTEGER PRIMARY KEY);\n" +
			"CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id));\n" +
			"INSERT INTO parent (id) VALUES (1);\n" +
			"INSERT INTO child (id, pid) VALUES (1, 1);\n" +
			"COMMIT;",
	}); err != nil {
		t.Fatalf("Apply seed: %v", err)
	}
	up := fkRebuild("parent", "CREATE TABLE parent_new (id INTEGER PRIMARY KEY);", false)
	err := a.Apply(ctx, &applyfetchpb.Migration{Id: "m2", UpSql: up})
	if err == nil {
		t.Fatal("B4: a rebuild that orphans rows of an existing key must fail (foreign_key_check), but Apply succeeded")
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

// A rebuild that renames a table in the same migration still knows the keys
// it carries: here the parent is renamed and then re-created without its
// rows, orphaning a child row the key held. The key now names the new parent,
// and must be recognised as the old key — the orphan is the rebuild's, not
// the data's, and fails the migration rather than being reported as a key
// added over existing rows.
func TestApply_RenameAndRebuildOrphaningAKey_Caught(t *testing.T) {
	a, ctx := fkApplier(t)
	if err := a.Apply(ctx, &applyfetchpb.Migration{
		Id: "m1",
		UpSql: "BEGIN;\n" +
			"CREATE TABLE parent (id INTEGER PRIMARY KEY);\n" +
			"CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id));\n" +
			"INSERT INTO parent (id) VALUES (1);\n" +
			"INSERT INTO child (id, pid) VALUES (1, 1);\n" +
			"COMMIT;",
	}); err != nil {
		t.Fatalf("Apply seed: %v", err)
	}
	up := "PRAGMA foreign_keys=OFF;\n\nBEGIN;\n\n" +
		"ALTER TABLE \"child\" RENAME TO \"kid\";\n" +
		"ALTER TABLE \"parent\" RENAME TO \"folk\";\n" +
		"CREATE TABLE \"folk__w17_new\" (id INTEGER PRIMARY KEY);\n" +
		"DROP TABLE \"folk\";\n" +
		"ALTER TABLE \"folk__w17_new\" RENAME TO \"folk\";\n\n" +
		"PRAGMA foreign_key_check;\n\nCOMMIT;\n\nPRAGMA foreign_keys=ON;\n"
	err := a.Apply(ctx, &applyfetchpb.Migration{Id: "m2", UpSql: up})
	if err == nil || !strings.Contains(err.Error(), "introduced foreign-key violation") || !strings.Contains(err.Error(), "kid(pid) -> folk(id)") {
		t.Fatalf("a rename+rebuild that orphans rows of a carried key must fail naming it, got: %v", err)
	}
}

// The other way round: a rename+rebuild that ADDS a key over orphans the
// renamed table already held reports them and applies.
func TestApply_RenameAndRebuildAddingAKeyOverOrphans_Reports(t *testing.T) {
	a, ctx := fkApplier(t)
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
	up := "PRAGMA foreign_keys=OFF;\n\nBEGIN;\n\n" +
		"ALTER TABLE \"child\" RENAME TO \"kid\";\n" +
		"CREATE TABLE \"kid__w17_new\" (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id));\n" +
		"INSERT INTO \"kid__w17_new\" SELECT * FROM \"kid\";\n" +
		"DROP TABLE \"kid\";\n" +
		"ALTER TABLE \"kid__w17_new\" RENAME TO \"kid\";\n\n" +
		"PRAGMA foreign_key_check;\n\nCOMMIT;\n\nPRAGMA foreign_keys=ON;\n"
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "m2", UpSql: up}); err != nil {
		t.Fatalf("a rename+rebuild adding a key over existing orphans must apply, got: %v", err)
	}
}
