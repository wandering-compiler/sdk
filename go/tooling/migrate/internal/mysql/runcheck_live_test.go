package mysql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// RunCheck counts what a check selects and refuses the round-5 attack (a
// multi-statement check that COMMITs out of the READ ONLY transaction and
// deletes), off a pool without multiStatements; a failure reports the error
// number only.
func TestRunCheck_CountsAndCannotWrite_Live(t *testing.T) {
	dsn, _ := liveMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: "CREATE TABLE `c` (id BIGINT PRIMARY KEY, pid BIGINT, s TEXT); INSERT INTO `c` VALUES (1, 7, 'secret-value'), (2, NULL, NULL);"}); err != nil {
		t.Fatal(err)
	}
	n, err := a.RunCheck(ctx, "SELECT count(*) FROM `c` WHERE `pid` IS NOT NULL LIMIT 1")
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
	for _, attack := range []string{
		"SELECT 0; COMMIT; DELETE FROM `c`; SELECT 0",
		"SELECT SLEEP(600)",
		"SELECT RELEASE_ALL_LOCKS()",
		"SELECT 1 INTO OUTFILE '/tmp/w17-check'",
		"SELECT /*! 1; DELETE FROM c */ 0",
	} {
		if _, err := a.RunCheck(ctx, attack); err == nil {
			t.Errorf("ran %q", attack)
		}
	}
	if n, err = a.RunCheck(ctx, "SELECT count(*) FROM `c`"); err != nil || n != 2 {
		t.Fatalf("rows after the refused writes: %d, %v", n, err)
	}
	_, err = a.RunCheck(ctx, "SELECT count(*) FROM `missing_secret_table` LIMIT 1")
	if err == nil || strings.Contains(err.Error(), "missing_secret") || !strings.Contains(err.Error(), "MySQL error 1146") {
		t.Fatalf("a failure must report only its error number: %v", err)
	}
	// the bodies' pool is untouched by a check's session
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: "INSERT INTO `c` VALUES (3, LENGTH(\"xy\"), NULL);"}); err != nil {
		t.Fatalf("an apply after the checks: %v", err)
	}
}
