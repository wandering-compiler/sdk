//go:build dockertest

package postgres_test

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wandering-compiler/sdk/go/tooling/fingerprint"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/internal/postgres"
)

// TestSnapshotter_RoundTrip is the S1 verify
// (`docs/specs/storage/dev-db-lifecycle.md`): dump → wipe → restore →
// fingerprint equal against a throwaway PG.
//
// Gated by `//go:build dockertest` (needs docker) and additionally
// skips when the pg_dump / psql client binaries are absent — the
// Snapshotter shells out to them, so the round-trip is only meaningful
// where they exist. Run via:
//
//	go test -tags=dockertest ./domains/console/apply/postgres/
func TestSnapshotter_RoundTrip(t *testing.T) {
	for _, bin := range []string{"pg_dump", "psql"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH — skipping snapshot round-trip", bin)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dsn := startThrowawayPG(ctx, t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	// Seed a non-trivial schema + data so the fingerprint has something
	// to compare and the dump carries rows, not just DDL.
	seed := []string{
		`CREATE TABLE widget (id BIGINT PRIMARY KEY, name TEXT NOT NULL, qty INT DEFAULT 0)`,
		`CREATE INDEX widget_name_idx ON widget (name)`,
		`INSERT INTO widget (id, name, qty) VALUES (1, 'a', 3), (2, 'b', 7)`,
	}
	for _, q := range seed {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	before, err := fingerprint.ExtractPostgres(ctx, conn)
	if err != nil {
		t.Fatalf("fingerprint before: %v", err)
	}

	snap, err := postgres.NewSnapshotter(dsn)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}

	// Dump.
	var dump bytes.Buffer
	if err := snap.Dump(ctx, &dump); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if dump.Len() == 0 {
		t.Fatal("Dump produced no output")
	}

	// Wipe: drop everything in the public schema.
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	wiped, err := fingerprint.ExtractPostgres(ctx, conn)
	if err != nil {
		t.Fatalf("fingerprint wiped: %v", err)
	}
	if wiped.FingerprintHex() == before.FingerprintHex() {
		t.Fatal("wipe did not change the schema fingerprint — setup is wrong")
	}

	// Restore.
	if err := snap.Restore(ctx, bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after, err := fingerprint.ExtractPostgres(ctx, conn)
	if err != nil {
		t.Fatalf("fingerprint after: %v", err)
	}
	if after.FingerprintHex() != before.FingerprintHex() {
		t.Fatalf("fingerprint mismatch after round-trip:\n before=%s\n  after=%s", before.FingerprintHex(), after.FingerprintHex())
	}

	// Spot-check the data came back too (fingerprint is schema-only).
	var qty int
	if err := conn.QueryRow(ctx, `SELECT qty FROM widget WHERE id = 2`).Scan(&qty); err != nil {
		t.Fatalf("post-restore data read: %v", err)
	}
	if qty != 7 {
		t.Errorf("restored qty = %d, want 7", qty)
	}
}

// startThrowawayPG runs a disposable postgres container, waits for it
// to accept connections, and returns a DSN pointing at it. The
// container is removed on test cleanup (--rm + explicit stop).
func startThrowawayPG(ctx context.Context, t *testing.T) string {
	t.Helper()
	const (
		image = "postgres:18-alpine"
		pass  = "snaptest"
		db    = "snaptest"
	)
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"-P",
		"-e", "POSTGRES_PASSWORD="+pass,
		"-e", "POSTGRES_DB="+db,
		image,
	).Output()
	if err != nil {
		t.Skipf("docker run failed (no docker?): %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_ = exec.Command("docker", "stop", id).Run()
	})

	// Resolve the host port mapped to 5432.
	portOut, err := exec.CommandContext(ctx, "docker", "port", id, "5432/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	// Lines look like "0.0.0.0:49153"; take the port after the last ':'.
	first := strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0]
	host := first[strings.LastIndex(first, ":")+1:]
	dsn := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%s/%s?sslmode=disable", pass, host, db)

	// Poll until ready.
	deadline := time.Now().Add(90 * time.Second)
	for {
		c, err := pgx.Connect(ctx, dsn)
		if err == nil {
			pingErr := c.Ping(ctx)
			_ = c.Close(context.Background())
			if pingErr == nil {
				return dsn
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres did not become ready: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled waiting for postgres: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
