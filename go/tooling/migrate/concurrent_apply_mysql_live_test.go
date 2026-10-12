package migrate_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/mysql"
)

// mysqlConcurrentDB creates a fresh database on W17_MYSQL_DSN's server and
// returns a URL DSN for it plus an admin handle; dropped on cleanup.
func mysqlConcurrentDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("W17_MYSQL_DSN")
	if dsn == "" {
		if os.Getenv("W17_REQUIRE_LIVE_MIGRATOR") == "1" {
			t.Fatal("W17_REQUIRE_LIVE_MIGRATOR=1 but W17_MYSQL_DSN is not set — refusing to skip-green")
		}
		t.Skip("W17_MYSQL_DSN not set — live MySQL test skipped")
	}
	driverDSN, err := mysql.URLToDriverDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", driverDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	db := fmt.Sprintf("conc_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + db) })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	return u.String(), admin
}

// Several replicas booting at once each run the same apply against one
// MySQL database. GET_LOCK serialises them: exactly one applies the
// migration; the others wait for the lock, find it applied and succeed.
// (MySQL commits DDL implicitly, so without the lock two runs would both
// apply the tail of the body.)
func TestApply_ConcurrentMigratorsApplyOnceOnMySQL_Live(t *testing.T) {
	dsn, admin := mysqlConcurrentDB(t)
	schema := strings.TrimPrefix(dsn[strings.LastIndex(dsn, "/"):], "/")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m := &applyfetchpb.Migration{
		Id:         "20260101T000000Z",
		Connection: "main",
		UpSql: `CREATE TABLE IF NOT EXISTS w17_migrations (timestamp VARCHAR(32) PRIMARY KEY, content_sha256 VARCHAR(64) NOT NULL);
			CREATE TABLE books (id BIGINT);
			INSERT INTO books VALUES (1);
			INSERT INTO w17_migrations VALUES ('20260101T000000Z', 'aa');`,
		DownSql: "DROP TABLE books;",
	}
	m.ContentSha256 = migrate.ContentHash(m.GetUpSql(), "", "", m.GetDownSql(), "", nil, "", "")
	dir := seedDirUnchained(t, m)

	const replicas = 4
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = migrate.Run(ctx, migrate.Config{
				Targets:       []migrate.ConnTarget{{Connection: "main", TargetMigrationID: m.GetId(), TargetContentSha256: m.GetContentSha256()}},
				MigrationsDir: dir,
				ApplierFor:    func(string) (migrate.Applier, error) { return mysql.New(ctx, dsn) },
				Out:           &strings.Builder{},
			})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: %v; a replica that lost the race must wait and succeed", i, err)
		}
	}
	var rows, ledger int
	if err := admin.QueryRow("SELECT COUNT(*) FROM " + schema + ".books").Scan(&rows); err != nil {
		t.Fatalf("books: %v", err)
	}
	if err := admin.QueryRow("SELECT COUNT(*) FROM " + schema + ".w17_migrations").Scan(&ledger); err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if rows != 1 || ledger != 1 {
		t.Errorf("books has %d row(s) and the ledger %d; the migration must be applied exactly once", rows, ledger)
	}
}
