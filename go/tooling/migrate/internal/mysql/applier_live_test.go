package mysql_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// liveMySQL creates a fresh database on the server W17_MYSQL_DSN (a
// mysql://user:pass@host:port/db URL) points at and returns a URL DSN for
// it plus an admin handle on the server; the database is dropped on cleanup.
func liveMySQL(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("W17_MYSQL_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_MYSQL_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_MYSQL_DSN not set — live MySQL applier test skipped")
	}
	driverDSN, err := mysql.URLToDriverDSN(dsn)
	if err != nil {
		t.Fatalf("W17_MYSQL_DSN: %v", err)
	}
	admin, err := sql.Open("mysql", driverDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := fmt.Sprintf("mig_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+db) })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("W17_MYSQL_DSN: %v", err)
	}
	u.Path = "/" + db
	return u.String(), admin
}

const mysqlLedgerCreate = `CREATE TABLE IF NOT EXISTS w17_migrations (
	timestamp VARCHAR(32) PRIMARY KEY, content_sha256 VARCHAR(64) NOT NULL);`

func mysqlMigration(id, table string) *applyfetchpb.Migration {
	return &applyfetchpb.Migration{
		Id: id,
		UpSql: mysqlLedgerCreate + `
			CREATE TABLE ` + table + ` (id BIGINT PRIMARY KEY);
			INSERT INTO w17_migrations VALUES ('` + id + `', 'aa');`,
		DownSql: `DROP TABLE ` + table + `;
			DELETE FROM w17_migrations WHERE timestamp = '` + id + `';`,
	}
}

func tableExists(t *testing.T, ctx context.Context, db *sql.DB, schema, table string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = ? AND table_name = ?`, schema, table).Scan(&n); err != nil {
		t.Fatalf("read tables: %v", err)
	}
	return n == 1
}

// Apply, the applied head and rollback against a real MySQL server: the
// multi-statement body runs whole, the head is the newest ledger row, and a
// rollback removes the migration's objects and returns the head.
func TestApplier_ApplyHeadRollback_Live(t *testing.T) {
	dsn, admin := liveMySQL(t)
	schema := strings.TrimPrefix(mustURL(t, dsn).Path, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	if head, err := a.AppliedHead(ctx); err != nil || head != "" {
		t.Fatalf("fresh AppliedHead = %q, %v; want empty", head, err)
	}
	first := mysqlMigration("20260101T000000Z", "authors")
	second := mysqlMigration("20260201T000000Z", "books")
	for _, m := range []*applyfetchpb.Migration{first, second} {
		if err := a.Apply(ctx, m); err != nil {
			t.Fatalf("Apply %s: %v", m.Id, err)
		}
	}
	if head, err := a.AppliedHead(ctx); err != nil || head != second.Id {
		t.Fatalf("AppliedHead = %q, %v; want %s", head, err, second.Id)
	}
	if !tableExists(t, ctx, admin, schema, "books") {
		t.Fatal("Apply did not run the whole body")
	}

	if err := a.Rollback(ctx, second); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if tableExists(t, ctx, admin, schema, "books") {
		t.Fatal("rollback left the migration's table behind")
	}
	if head, err := a.AppliedHead(ctx); err != nil || head != first.Id {
		t.Fatalf("AppliedHead after rollback = %q, %v; want %s", head, err, first.Id)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The run lock is held on one dedicated session. When that session is
// killed the server releases the lock without telling anyone; Check — asked
// before every migration — and Release must both report the loss.
func TestRunLock_CheckReportsALostLockSession_Live(t *testing.T) {
	dsn, admin := liveMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	rl, err := a.AcquireRunLock(ctx)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	lock, ok := rl.(migrate.CheckedRunLock)
	if !ok {
		t.Fatalf("the MySQL run lock (%T) is not a CheckedRunLock", rl)
	}
	if err := lock.Check(ctx); err != nil {
		t.Fatalf("Check on a held lock = %v; want nil", err)
	}

	// Kill the session that holds it, and let a second run take it.
	var holder int64
	if err := admin.QueryRowContext(ctx, `SELECT IS_USED_LOCK(lock_name) FROM (
		SELECT object_name AS lock_name FROM performance_schema.metadata_locks
		WHERE object_type = 'USER LEVEL LOCK') l LIMIT 1`).Scan(&holder); err != nil {
		t.Fatalf("find the lock session: %v", err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("KILL %d", holder)); err != nil {
		t.Fatalf("KILL: %v", err)
	}
	b, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New (second run): %v", err)
	}
	defer func() { _ = b.Close() }()
	var second migrate.RunLock
	deadline := time.Now().Add(10 * time.Second)
	for {
		second, err = b.AcquireRunLock(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second AcquireRunLock after the holder was killed: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer func() { _ = second.Release(context.Background()) }()

	if err := lock.Check(ctx); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Check after the lock session was killed = %v; want ErrLockLost", err)
	}
	if err := lock.Release(ctx); !errors.Is(err, migrate.ErrLockLost) {
		t.Fatalf("Release after the lock session was killed = %v; want ErrLockLost", err)
	}
}

// Reading the applied head is what `status` and every plan do: it must not
// wait behind a migration in flight, and it must not change the ledger.
func TestAppliedHead_IsReadOnlyAndDoesNotWaitBehindAMigration_Live(t *testing.T) {
	dsn, admin := liveMySQL(t)
	schema := strings.TrimPrefix(mustURL(t, dsn).Path, "/")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	// A fresh database: reading the head creates nothing.
	if head, err := a.AppliedHead(ctx); err != nil || head != "" {
		t.Fatalf("fresh AppliedHead = %q, %v", head, err)
	}
	if tableExists(t, ctx, admin, schema, "w17_migrations") {
		t.Fatal("reading the head created the ledger")
	}

	first := mysqlMigration("20260101T000000Z", "authors")
	if err := a.Apply(ctx, first); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// A migration in flight: its transaction has written the next ledger
	// row and holds it until it commits.
	inflight, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inflight.Close() }()
	if _, err := inflight.ExecContext(ctx, "START TRANSACTION"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = inflight.ExecContext(context.Background(), "ROLLBACK") }()
	if _, err := inflight.ExecContext(ctx, "INSERT INTO "+schema+".w17_migrations VALUES ('20260201T000000Z', 'bb')"); err != nil {
		t.Fatalf("in-flight ledger write: %v", err)
	}

	probe, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
	head, err := a.AppliedHead(probe)
	cancelProbe()
	if err != nil {
		t.Fatalf("AppliedHead behind an in-flight migration = %v; a read must not queue behind the ledger's writers", err)
	}
	if head != first.Id {
		t.Errorf("head = %q, want %s (the in-flight row is not applied yet)", head, first.Id)
	}
}

// MySQL cannot resume a post-transaction half: the applier does not offer
// the resume capability, and running a non-empty one warns, naming the
// migration; a migration without one says nothing.
func TestApply_PostTxWarnsThatItIsNotResumable_Live(t *testing.T) {
	dsn, _ := liveMySQL(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := mysql.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if _, ok := any(a).(migrate.ResumableApplier); ok {
		t.Error("the MySQL applier claims it can resume a post-transaction half")
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "20260101T000000Z", UpSql: "CREATE TABLE t (id BIGINT);"}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a migration without a post-transaction half warned: %q", buf.String())
	}
	if err := a.Apply(ctx, &applyfetchpb.Migration{Id: "20260201T000000Z", UpSql: "CREATE TABLE u (id BIGINT);", UpPostTx: "CREATE INDEX u_id ON u (id);"}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "NOT resumable") || !strings.Contains(out, "20260201T000000Z") {
		t.Errorf("no not-resumable warning naming the migration: %q", out)
	}
}
