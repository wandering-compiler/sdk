package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// A migration body carries its own BEGIN … COMMIT. When a statement in it
// fails, the transaction it opened must not stay on the pooled connection:
// the next migration on the same applier used to fail with "cannot start a
// transaction within a transaction" (or wait on the lock it held). The
// failed body leaves nothing behind, and the applier carries on — after a
// failed up_sql, and after a failed down_sql.
func TestApply_AFailedBodyLeavesNoTransactionOpen(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, "sqlite://"+filepath.Join(t.TempDir(), "rb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	failing := &applyfetchpb.Migration{Id: "m1", UpSql: "BEGIN;\nCREATE TABLE half (x INTEGER);\nINSERT INTO nowhere VALUES (1);\nCOMMIT;\n"}
	if err := a.Apply(ctx, failing); err == nil || !strings.Contains(err.Error(), "up_sql") {
		t.Fatalf("a failing body applied: %v", err)
	}
	ok := &applyfetchpb.Migration{Id: "m2",
		UpSql:   "BEGIN;\nCREATE TABLE whole (x INTEGER);\nCOMMIT;\n",
		DownSql: "BEGIN;\nDROP TABLE whole;\nINSERT INTO nowhere VALUES (1);\nCOMMIT;\n"}
	if err := a.Apply(ctx, ok); err != nil {
		t.Fatalf("the next migration after a failed one: %v", err)
	}
	if n := countTables(t, a, "half"); n != 0 {
		t.Errorf("the failed body's table is there (%d): its transaction was not rolled back", n)
	}

	if err := a.Rollback(ctx, ok); err == nil {
		t.Fatal("a failing down applied")
	}
	if n := countTables(t, a, "whole"); n != 1 {
		t.Errorf("the failed down dropped the table (%d): its transaction was not rolled back", n)
	}
	next := &applyfetchpb.Migration{Id: "m3", UpSql: "BEGIN;\nCREATE TABLE after (x INTEGER);\nCOMMIT;\n"}
	if err := a.Apply(ctx, next); err != nil {
		t.Fatalf("the next migration after a failed rollback: %v", err)
	}
}

func countTables(t *testing.T, a *Applier, name string) int {
	t.Helper()
	var n int
	if err := a.db.QueryRowContext(context.Background(), `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
