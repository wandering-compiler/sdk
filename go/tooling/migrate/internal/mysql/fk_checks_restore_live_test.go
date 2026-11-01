package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// A key added over a table that holds orphans runs with FOREIGN_KEY_CHECKS
// off for that one statement (the migrator's shape: save, off, ADD, restore).
// The body applies over the orphan, and the session it ran on — handed back to
// the pool on success — has the checks on again: the next statement on that
// same connection refuses a new orphan. Live MySQL (W17_MYSQL_DSN).
func TestApply_FKChecksRestoredAfterASuccessfulBody(t *testing.T) {
	dsn := os.Getenv("W17_MYSQL_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_MYSQL_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_MYSQL_DSN not set — live MySQL applier test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	driverDSN, err := URLToDriverDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", driverDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("mig_fkoff_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name) }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	a, err := New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	a.db.SetMaxOpenConns(1) // every statement below runs on the session the body used

	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: "CREATE TABLE p (id BIGINT PRIMARY KEY);\n" +
		"CREATE TABLE c (id BIGINT PRIMARY KEY, pid BIGINT);\n" +
		"INSERT INTO p VALUES (1);\nINSERT INTO c VALUES (1, 1), (2, 999);"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	add := "SET @w17_fk_checks = @@FOREIGN_KEY_CHECKS;\nSET FOREIGN_KEY_CHECKS = 0;\n" +
		"ALTER TABLE `c` ADD CONSTRAINT `c_pid_fkey` FOREIGN KEY (`pid`) REFERENCES `p`(`id`);\n" +
		"SET FOREIGN_KEY_CHECKS = @w17_fk_checks;"
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: add}); err != nil {
		t.Fatalf("the key over an orphan must apply: %v", err)
	}
	var on int
	if err := a.db.QueryRowContext(ctx, "SELECT @@SESSION.FOREIGN_KEY_CHECKS").Scan(&on); err != nil || on != 1 {
		t.Fatalf("FOREIGN_KEY_CHECKS after the body = %d (%v), want 1", on, err)
	}
	if _, err := a.db.ExecContext(ctx, "INSERT INTO c VALUES (3, 998)"); err == nil {
		t.Fatal("a new orphan was accepted after the key was added")
	}
	var orphans int
	if err := a.db.QueryRowContext(ctx, "SELECT count(*) FROM c WHERE pid NOT IN (SELECT id FROM p)").Scan(&orphans); err != nil || orphans != 1 {
		t.Fatalf("orphans = %d (%v), want the one that was there", orphans, err)
	}
}
