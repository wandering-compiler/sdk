package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// A migration body carries its own BEGIN … COMMIT. When a statement in it
// fails, the session sits in an aborted transaction: the next migration on
// the same applier used to fail with 25P02 ("current transaction is
// aborted"). The failed body leaves nothing behind, and the applier carries
// on — after a failed up_sql, and after a failed down_sql.
func TestApply_AFailedBodyLeavesNoTransactionOpen_Live(t *testing.T) {
	dsn := os.Getenv("W17_PG_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_PG_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_PG_DSN not set — live Postgres applier test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() }) // after the drop below: cleanups run last-first
	sfx := time.Now().UnixNano()
	half, whole, after := fmt.Sprintf("w17_rb_half_%d", sfx), fmt.Sprintf("w17_rb_whole_%d", sfx), fmt.Sprintf("w17_rb_after_%d", sfx)
	t.Cleanup(func() {
		_, _ = a.conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+half+", "+whole+", "+after)
	})
	exists := func(name string) bool {
		var ok bool
		if err := a.conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
			t.Fatalf("probe %s: %v", name, err)
		}
		return ok
	}

	failing := &applyfetchpb.Migration{Id: "m1", UpSql: "BEGIN;\nCREATE TABLE " + half + " (x int);\nINSERT INTO w17_rb_nowhere VALUES (1);\nCOMMIT;\n"}
	if err := a.Apply(ctx, failing); err == nil || !strings.Contains(err.Error(), "up_sql") {
		t.Fatalf("a failing body applied: %v", err)
	}
	ok := &applyfetchpb.Migration{Id: "m2",
		UpSql:   "BEGIN;\nCREATE TABLE " + whole + " (x int);\nCOMMIT;\n",
		DownSql: "BEGIN;\nDROP TABLE " + whole + ";\nINSERT INTO w17_rb_nowhere VALUES (1);\nCOMMIT;\n"}
	if err := a.Apply(ctx, ok); err != nil {
		t.Fatalf("the next migration after a failed one: %v", err)
	}
	if exists(half) {
		t.Errorf("the failed body's table %s is there", half)
	}
	if err := a.Rollback(ctx, ok); err == nil {
		t.Fatal("a failing down applied")
	}
	if !exists(whole) {
		t.Errorf("the failed down dropped %s: its transaction was not rolled back", whole)
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "m3", UpSql: "BEGIN;\nCREATE TABLE " + after + " (x int);\nCOMMIT;\n"}); err != nil {
		t.Fatalf("the next migration after a failed rollback: %v", err)
	}
}
