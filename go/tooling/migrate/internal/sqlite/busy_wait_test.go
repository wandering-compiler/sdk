package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/sqlite"
)

// SQLite has no run lock; what keeps an apply from failing on another
// writer's open transaction is the busy timeout. Without one the driver
// answers SQLITE_BUSY ("database is locked") the moment the body needs the
// write lock, and the failure reads as a broken migration.
func TestApply_WaitsForAnotherWritersTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	a, err := sqlite.New(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = a.Close() }()

	other, err := sql.Open("sqlite", sqlite.URLToDriverDSN("sqlite://"+path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	ctx := context.Background()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("other writer BEGIN IMMEDIATE: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(time.Second)
		_, _ = conn.ExecContext(ctx, "COMMIT")
		close(released)
	}()

	err = a.Apply(ctx, &applyfetchpb.Migration{
		Id:    "ts-1",
		UpSql: "BEGIN; CREATE TABLE users (id INTEGER PRIMARY KEY); COMMIT;",
	})
	<-released
	if err != nil {
		t.Fatalf("Apply while another writer held the database for a second = %v; want it to wait", err)
	}
}
