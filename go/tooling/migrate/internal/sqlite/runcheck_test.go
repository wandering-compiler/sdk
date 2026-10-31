package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// RunCheck counts what a check selects, cannot write (query_only), and hands
// the connection back writable.
func TestRunCheck_CountsAndCannotWrite(t *testing.T) {
	ctx := context.Background()
	a, err := sqlite.New(ctx, "sqlite://"+filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: `CREATE TABLE "c" (id INTEGER PRIMARY KEY, pid INTEGER); INSERT INTO "c" VALUES (1, 7), (2, NULL);`}); err != nil {
		t.Fatal(err)
	}
	n, err := a.RunCheck(ctx, `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`)
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
	if _, err := a.RunCheck(ctx, `DELETE FROM "c" RETURNING 1`); err == nil {
		t.Fatal("a check wrote: query_only did not hold")
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: `INSERT INTO "c" VALUES (3, 1);`}); err != nil {
		t.Fatalf("the connection stayed read-only after a check: %v", err)
	}
	if n, err = a.RunCheck(ctx, `SELECT count(*) FROM "c"`); err != nil || n != 3 {
		t.Fatalf("rows: %d, %v; want 3", n, err)
	}
}
