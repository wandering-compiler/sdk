package postgres_test

import (
	"context"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// RunCheck counts what a pre-apply check selects, and cannot write: a check
// is not covered by the content hash, so it runs in a READ ONLY transaction.
func TestRunCheck_CountsAndCannotWrite_Live(t *testing.T) {
	dsn := liveDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: `CREATE TABLE "c" (id BIGINT PRIMARY KEY, pid BIGINT); INSERT INTO "c" VALUES (1, 7), (2, NULL);`}); err != nil {
		t.Fatal(err)
	}
	n, err := a.RunCheck(ctx, `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`)
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
	if _, err := a.RunCheck(ctx, `WITH d AS (DELETE FROM "c" RETURNING 1) SELECT count(*) FROM d`); err == nil {
		t.Fatal("a check wrote: the READ ONLY transaction did not hold")
	}
	if n, err = a.RunCheck(ctx, `SELECT count(*) FROM "c"`); err != nil || n != 2 {
		t.Fatalf("rows after the refused write: %d, %v", n, err)
	}
}
