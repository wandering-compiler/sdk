package migrate_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// Several replicas booting at once each run the same apply against one
// Postgres database. Exactly one applies the migration. Each of the others
// either finds it applied or fails fast on the run lock, naming the run in
// progress — never on what the migration's own body ran into (a catalog
// collision on the table it creates reads as a broken migration).
func TestApply_ConcurrentMigratorsApplyOnce_Live(t *testing.T) {
	dsn := fakePGDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m := &applyfetchpb.Migration{
		Id:         "20260101T000000Z",
		Connection: "main",
		UpSql: `BEGIN;
			CREATE TABLE IF NOT EXISTS w17_migrations (timestamp TIMESTAMPTZ PRIMARY KEY, content_sha256 TEXT NOT NULL, post_tx_complete BOOLEAN NOT NULL DEFAULT true);
			CREATE TABLE books (id BIGINT);
			INSERT INTO books VALUES (1);
			INSERT INTO w17_migrations VALUES ('2026-01-01T00:00:00Z', 'aa', true);
			COMMIT;`,
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
				ApplierFor:    func(string) (migrate.Applier, error) { return postgres.New(ctx, dsn) },
				Out:           &strings.Builder{},
			})
		}(i)
	}
	close(start)
	wg.Wait()
	succeeded := 0
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case !strings.Contains(err.Error(), "another migration run is in progress"):
			t.Errorf("replica %d failed on something other than the run lock: %v", i, err)
		}
	}
	if succeeded == 0 {
		t.Error("no replica applied the migration")
	}

	probe, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close(context.Background()) }()
	var rows, ledger int
	if err := probe.QueryRow(ctx, "SELECT count(*) FROM books").Scan(&rows); err != nil {
		t.Fatalf("books: %v", err)
	}
	if err := probe.QueryRow(ctx, "SELECT count(*) FROM w17_migrations").Scan(&ledger); err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if rows != 1 || ledger != 1 {
		t.Errorf("books has %d row(s) and the ledger %d; the migration must be applied exactly once", rows, ledger)
	}
}
