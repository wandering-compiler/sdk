package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// RunCheck counts what a check selects, refuses the round-5 attacks (PRAGMA
// query_only = OFF then a write; ATTACH creating a file), and hands the
// connection back writable.
func TestRunCheck_CountsAndCannotWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, err := sqlite.New(ctx, "sqlite://"+filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: `CREATE TABLE "c" (id INTEGER PRIMARY KEY, pid INTEGER); INSERT INTO "c" VALUES (1, 7), (2, NULL);`}); err != nil {
		t.Fatal(err)
	}
	n, err := a.RunCheck(ctx, `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`, "")
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
	attached := filepath.Join(dir, "attached.db")
	for _, attack := range []string{
		`PRAGMA query_only = OFF; DELETE FROM "c"`,
		`SELECT 0; PRAGMA query_only = OFF; DELETE FROM "c"`,
		`ATTACH '` + attached + `' AS x`,
		`SELECT 0; ATTACH '` + attached + `' AS x`,
		`DELETE FROM "c" RETURNING 1`,
		`SELECT load_extension('x')`,
	} {
		if _, err := a.RunCheck(ctx, attack, ""); err == nil {
			t.Errorf("ran %q", attack)
		}
	}
	if _, err := os.Stat(attached); err == nil {
		t.Fatal("a check created a file")
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: `INSERT INTO "c" VALUES (3, 1);`}); err != nil {
		t.Fatalf("the connection stayed read-only after a check: %v", err)
	}
	if n, err = a.RunCheck(ctx, `SELECT count(*) FROM "c"`, ""); err != nil || n != 3 {
		t.Fatalf("rows: %d, %v; want 3", n, err)
	}
}
