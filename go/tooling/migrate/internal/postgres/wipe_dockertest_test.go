//go:build dockertest

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// TestWipe_DropsEverything — Wipe (DROP SCHEMA public CASCADE) empties
// the DB. Gated //go:build dockertest.
func TestWipe_DropsEverything(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn := startThrowawayPG(ctx, t)

	a, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	if err := a.Apply(ctx, &applyfetchpb.Migration{UpSql: `CREATE TABLE a(id BIGINT PRIMARY KEY); CREATE TABLE b(id BIGINT PRIMARY KEY, a_id BIGINT REFERENCES a(id));`}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := a.Wipe(ctx); err != nil {
		t.Fatalf("Wipe: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("after Wipe %d tables remain in public, want 0", n)
	}
}
