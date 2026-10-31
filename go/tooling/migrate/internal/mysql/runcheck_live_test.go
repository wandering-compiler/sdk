package mysql_test

import (
	"context"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// RunCheck runs the planner's double-quoted check (ANSI_QUOTES) and counts
// what it selects; it cannot write (READ ONLY transaction), and the session
// mode does not leak into the pool.
func TestRunCheck_CountsAndCannotWrite_Live(t *testing.T) {
	dsn, _ := liveMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: "CREATE TABLE `c` (id BIGINT PRIMARY KEY, pid BIGINT); INSERT INTO `c` VALUES (1, 7), (2, NULL);"}); err != nil {
		t.Fatal(err)
	}
	n, err := a.RunCheck(ctx, `SELECT count(*) FROM "c" WHERE "pid" IS NOT NULL LIMIT 1`)
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
	// multiStatements is on: a smuggled write runs inside the READ ONLY
	// transaction and is refused (1792)
	if _, err := a.RunCheck(ctx, `DELETE FROM "c"; SELECT 1`); err == nil {
		t.Fatal("a check wrote: the READ ONLY transaction did not hold")
	}
	if n, err = a.RunCheck(ctx, `SELECT count(*) FROM "c"`); err != nil || n != 2 {
		t.Fatalf("rows after the refused write: %d, %v", n, err)
	}
	// a plain apply after a check still parses backticks and treats "x" as a
	// string, i.e. ANSI_QUOTES did not stay on a pooled connection
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: "INSERT INTO `c` VALUES (3, LENGTH(\"xy\"));"}); err != nil {
		t.Fatalf("the session mode of a check leaked into an apply: %v", err)
	}
}
